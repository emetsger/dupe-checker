// Traverses PASS repository resources by following LDP containment relationships.  Retrieval of repository resources
// occurs in parallel.  Callers are expected to launch a Visitor in a background goroutine, and read accepted resources
// and errors off of channels in separate goroutines.
package visit

import (
	"dupe-checker/model"
	"dupe-checker/retriever"
	"fmt"
	"log"
	"sync"
)

type Visitor struct {
	// retrieves LDP containers; invocation is gated by the semaphore
	retriever retriever.Retriever
	// gates the maximum number of requests which may be performed in parallel
	semaphore chan int
	// Resources which are accepted are written to this channel
	Containers chan model.LdpContainer
	// Any errors encountered when traversing the repository are written to this channel
	Errors chan error
}

type VisitErr struct {
	Uri     string
	Message string
	Wrapped error
}

// descends into every container
var defaultFilter = func(c model.LdpContainer) bool { return true }

// accepts every PASS resource
var defaultAccept = func(c model.LdpContainer) bool {
	if isPass, _ := c.IsPassResource(); isPass {
		if len(c.Uri()) > 0 {
			return true
		}
	}
	return false
}

func (ve VisitErr) Error() string {
	return fmt.Sprintf("visit: error visiting uri %s, %s", ve.Uri, ve.Message)
}

func (ve VisitErr) Unwrap() error {
	return ve.Wrapped
}

// Constructs a new Visitor instance using the supplied Retriever.  At most maxConcurrent requests are performed in
// parallel.
func New(retriever retriever.Retriever, maxConcurrent int) Visitor {
	return Visitor{
		retriever:  retriever,
		semaphore:  make(chan int, maxConcurrent),
		Containers: make(chan model.LdpContainer),
		Errors:     make(chan error),
	}
}

// Given a starting URI, test each contained resource for recursion using the supplied filter.  Recurse into filtered
// resources and test each resource for acceptance.  Accepted resources will be written to the Containers channel.  Note
// the resource provided by the starting URI is tested for acceptance.
//
// This function blocks until all messages have been read off of the Errors and Containers channel.  Typically
// Walk should be invoked within a goroutine while the Errors and Containers channel are read in separate goroutines.
//
// Both filter and accept may be nil, in which case all resources are filtered for recursion, and all PASS resources are
// accepted.
func (v Visitor) Walk(startUri string, filter, accept func(container model.LdpContainer) bool) {
	var c model.LdpContainer
	var e error

	if c, e = v.retriever.Get(startUri); e != nil {
		log.Fatalf("visit: error retrieving %s: %s", startUri, e.Error())
		return
	}

	if c.Uri() == "" {
		log.Fatalf("visit: missing container for %s", startUri)
		return
	}

	if filter == nil {
		filter = defaultFilter
	}

	if accept == nil {
		accept = defaultAccept
	}

	if accept(c) {
		v.Containers <- c
	}

	v.walkInternal(c, filter, accept)

	close(v.Containers)
	close(v.Errors)
}

func (v Visitor) walkInternal(c model.LdpContainer, filter, accept func(container model.LdpContainer) bool) {
	var e error
	wg := sync.WaitGroup{}
	wg.Add(len(c.Contains()))
	for _, uri := range c.Contains() {
		v.semaphore <- 1
		go func(uri string) {
			log.Printf("visit: retrieving %s", uri)
			if c, e = v.retriever.Get(uri); e != nil {
				<-v.semaphore
				v.Errors <- fmt.Errorf("%v", VisitErr{
					Uri:     uri,
					Message: e.Error(),
					Wrapped: e,
				})
			} else {
				<-v.semaphore
				if accept(c) {
					v.Containers <- c
				}
				if filter(c) {
					v.walkInternal(c, filter, accept)
				}
			}
			wg.Done()
		}(uri)
	}
	wg.Wait()
}
