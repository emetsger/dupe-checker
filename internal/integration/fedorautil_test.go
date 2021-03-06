//
// Copyright 2021 Johns Hopkins University
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// +build integration

package integration

import (
	"bytes"
	"dupe-checker/env"
	"dupe-checker/model"
	"fmt"
	"github.com/knakk/rdf"
	"github.com/stretchr/testify/assert"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
)

type tripleTransformer func(trips *[]*rdf.Triple)

var noopTripleTransformer = tripleTransformer(func(trips *[]*rdf.Triple) { /* noop*/ })

func compositeTransformer(transformFunc ...tripleTransformer) tripleTransformer {
	return func(trips *[]*rdf.Triple) {
		for _, transformer := range transformFunc {
			transformer(trips)
		}
	}
}

// Replaces the subject of all triples when the subject IRI equals 'replace' with 'replacement'.  As a special case, if
// 'replace' is '*', then all subjects are replaced with performing any equality comparison
func subjectTransformer(replace, replacement string) tripleTransformer {
	return func(trips *[]*rdf.Triple) {
		subjIri, _ := rdf.NewIRI(replacement)
		for i, trip := range *trips {
			if trip == nil {
				continue
			}
			if trip.Subj.String() == replace || replace == "*" {
				(*trips)[i] = &rdf.Triple{subjIri, trip.Pred, trip.Obj}
			}
		}
	}
}

// Replaces the object of all triples with the predicate http://oapass.org/ns/pass#publication when the object
// equals 'replace' with 'replacement'.  If 'replace' equals the special case '*', then 'replacement' is set as the
// value without performing any equality comparison.
func publicationTransformer(replace, replacement string) tripleTransformer {
	return func(trips *[]*rdf.Triple) {
		for i, trip := range *trips {
			if trip == nil {
				continue
			}
			if trip.Pred.String() == fmt.Sprintf("%s%s", model.PassResourceUriPrefix, "publication") &&
				(trip.Obj.String() == replace || replace == "*") {
				log.Printf("transforming submission publication %s to %s", replace, replacement)
				pubIri, _ := rdf.NewIRI(replacement)
				(*trips)[i] = &rdf.Triple{trip.Subj, trip.Pred, pubIri}
			}
		}
	}
}

// Removes all triples with rdf:type not prefixed by "http://oapass.org/ns/pass#"
func passTypeFilter() tripleTransformer {
	return func(trips *[]*rdf.Triple) {
		for i, trip := range *trips {
			if trip == nil {
				continue
			}
			if (*trip).Pred.String() == model.RdfTypeUri {
				if !strings.HasPrefix((*trip).Obj.String(), model.PassResourceUriPrefix) {
					(*trips)[i] = nil
				}
			}
		}
	}
}

// Removes all triples that do not have a predicate prefixed by "http://oapass.org/ns/pass#", or triples that do not
// have an object prefixed by "http://oapass.org/ns/pass#"
func passPredicateAndObjectFilter() tripleTransformer {
	return func(trips *[]*rdf.Triple) {
		for i, trip := range *trips {
			if trip == nil {
				continue
			}
			if !strings.HasPrefix(trip.Pred.String(), model.PassResourceUriPrefix) &&
				!strings.HasPrefix(trip.Obj.String(), model.PassResourceUriPrefix) {
				(*trips)[i] = nil
			}
		}
	}
}

// Culls nil elements from the provided slice
func removeNils(trips *[]*rdf.Triple) []*rdf.Triple {
	var result []*rdf.Triple
	for i := range *trips {
		if (*trips)[i] != nil {
			result = append(result, (*trips)[i])
		}
	}

	return result
}

func copyTriples(source []rdf.Triple, transformer tripleTransformer) []rdf.Triple {
	var result []rdf.Triple

	if transformer == nil {
		for _, triple := range source {
			result = append(result, triple)
		}
		return result
	}

	transformable := make([]*rdf.Triple, len(source))

	for i, triple := range source {
		triple := triple
		transformable[i] = &triple
	}

	transformer(&transformable)
	nilSafe := removeNils(&transformable)

	for _, triple := range nilSafe {
		result = append(result, *triple)
	}

	return result
}

// Copies the content from sourceUri, transforms the triples, and PUTs a new resource with the transformed content
// at targetUri.
//
// The only valid combination to perform a copy of an RDF resource with knakk/rdf is to use N-Triples serialization in
// combination with PUT.  It is particularly difficult to transform the RDF of the resource to be copied, especially
// because knakk/rdf does not allow for null relative URIs.
func copyFedoraResource(t *testing.T, environment env.Env, sourceUri, targetUri string, transformer func(triples *[]*rdf.Triple)) string {
	var req *http.Request
	var res *http.Response
	var trips []rdf.Triple
	var err error

	req, err = http.NewRequest("GET", sourceUri, nil)
	assert.Nil(t, err)
	req.SetBasicAuth(environment.FcrepoUser, environment.FcrepoPassword)
	req.Header.Add("Accept", "application/n-triples")

	res, err = httpClient.Do(req)
	defer func() { _ = res.Body.Close() }()
	assert.Nil(t, err)

	b := bytes.Buffer{}
	_, err = io.Copy(&b, res.Body)
	assert.Nil(t, err)
	assert.True(t, res.StatusCode == 200, err)

	var filteredTrips []*rdf.Triple
	filteredResource := bytes.Buffer{}
	trips, err = rdf.NewTripleDecoder(&b, rdf.Turtle).DecodeAll()
	assert.True(t, len(trips) > 0)

	for i := range trips {
		t := trips[i]
		if strings.HasPrefix(t.Pred.String(), model.PassResourceUriPrefix) ||
			strings.HasPrefix(t.Obj.String(), model.PassResourceUriPrefix) {
			filteredTrips = append(filteredTrips, &t)
		}
	}

	// replace the subject
	subject, _ := rdf.NewIRI(targetUri)
	for i := range filteredTrips {
		filteredTrips[i] = &rdf.Triple{Subj: subject, Pred: filteredTrips[i].Pred, Obj: filteredTrips[i].Obj}
	}

	transformer(&filteredTrips)
	filteredTrips = removeNils(&filteredTrips)

	filteredResource = bytes.Buffer{}
	encoder := rdf.NewTripleEncoder(&filteredResource, rdf.NTriples)
	var toEncode []rdf.Triple
	for _, triple := range filteredTrips {
		toEncode = append(toEncode, *triple)
	}
	err = encoder.EncodeAll(toEncode)
	assert.Nil(t, err)
	_ = encoder.Close()

	log.Printf("Replaced copy:\n%s", filteredResource.String())

	req, err = http.NewRequest("PUT", targetUri, bytes.NewReader(filteredResource.Bytes()))
	assert.Nil(t, err)
	req.SetBasicAuth(environment.FcrepoUser, environment.FcrepoPassword)
	req.Header.Add("Content-Type", "application/n-triples")

	res, err = httpClient.Do(req)
	defer func() { _ = res.Body.Close() }()
	assert.Nil(t, err)
	assert.True(t, res.StatusCode == 201, fmt.Sprintf("status code: %v, error: %v", res.StatusCode, err))
	b = bytes.Buffer{}
	_, err = io.Copy(&b, res.Body)
	return string(b.Bytes())
}

// Attempts to replace the content of the Fedora resource at 'uri' with the content in 'body' described by 'mediaType'.
// Note: if a resource already exists at 'uri', this func will DELETE it, and then PUT a new resource; SPARQL update is
// not used.
func replaceFedoraResource(t *testing.T, environment env.Env, uri string, body []byte, mediaType string) (err error) {
	var (
		req *http.Request
		res *http.Response
	)

	// Delete the resource at 'uri' if it exists, and get rid of the tombstone.
	if req, err = http.NewRequest("HEAD", uri, nil); err != nil {
		return err
	}
	req.SetBasicAuth(environment.FcrepoUser, environment.FcrepoPassword)
	if res, err = httpClient.Do(req); err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode <= 300 {
		// delete the existing resource
		if req, err = http.NewRequest("DELETE", uri, nil); err != nil {
			return err
		}
		req.SetBasicAuth(environment.FcrepoUser, environment.FcrepoPassword)
		if res, err = httpClient.Do(req); err != nil {
			return err
		}
		defer res.Body.Close()
		if req, err = http.NewRequest("DELETE", fmt.Sprintf("%s/fcr:tombstone", uri), nil); err != nil {
			return err
		}
		req.SetBasicAuth(environment.FcrepoUser, environment.FcrepoPassword)
		if res, err = httpClient.Do(req); err != nil {
			return err
		}
		defer res.Body.Close()
	}

	// PUT the 'body' at 'uri'
	if req, err = http.NewRequest("PUT", uri, bytes.NewReader(body)); err != nil {
		return err
	}
	req.SetBasicAuth(environment.FcrepoUser, environment.FcrepoPassword)
	req.Header.Add("Content-Type", mediaType)
	if res, err = httpClient.Do(req); err != nil {
		return err
	}
	defer res.Body.Close()

	assert.True(t, res.StatusCode < 300, "%s %s %s: %v", "PUT", uri, res.StatusCode, res.Body)
	return err
}
