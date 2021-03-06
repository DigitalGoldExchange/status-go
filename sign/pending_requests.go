package sign

import (
	"context"
	"sync"
	"time"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/status-im/status-go/geth/account"
)

type verifyFunc func(string) (*account.SelectedExtKey, error)

// PendingRequests is a capped container that holds pending signing requests.
type PendingRequests struct {
	mu       sync.RWMutex // to guard transactions map
	requests map[string]*Request

	log log.Logger
}

// NewPendingRequests creates a new requests list
func NewPendingRequests() *PendingRequests {
	logger := log.New("package", "status-go/sign.PendingRequests")

	return &PendingRequests{
		requests: make(map[string]*Request),
		log:      logger,
	}
}

// Add a new signing request.
func (rs *PendingRequests) Add(ctx context.Context, meta Meta, completeFunc completeFunc) (*Request, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	request := newRequest(ctx, meta, completeFunc)
	rs.requests[request.ID] = request
	rs.log.Info("signing request is created", "ID", request.ID)

	go NotifyOnEnqueue(request)

	return request, nil
}

// Get returns a signing request by it's ID.
func (rs *PendingRequests) Get(id string) (*Request, error) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	if request, ok := rs.requests[id]; ok {
		return request, nil
	}
	return nil, ErrSignReqNotFound
}

// First returns a first signing request (if exists, nil otherwise).
func (rs *PendingRequests) First() *Request {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	for _, req := range rs.requests {
		return req
	}

	return nil
}

// Approve a signing request by it's ID. Requires a valid password and a verification function.
func (rs *PendingRequests) Approve(id string, password string, verify verifyFunc) (hash gethcommon.Hash, err error) {
	rs.log.Info("complete transaction", "id", id)
	request, err := rs.tryLock(id)
	if err != nil {
		rs.log.Warn("can't process transaction", "err", err)
		return hash, err
	}

	selectedAccount, err := verify(password)
	if err != nil {
		rs.complete(request, hash, err)
		return hash, err
	}

	hash, err = request.completeFunc(selectedAccount)
	rs.log.Info("finally completed transaction", "id", request.ID, "hash", hash, "err", err)

	rs.complete(request, hash, err)

	return hash, err
}

// Discard remove a signing request from the list of pending requests.
func (rs *PendingRequests) Discard(id string) error {
	request, err := rs.Get(id)
	if err != nil {
		return err
	}

	rs.complete(request, gethcommon.Hash{}, ErrSignReqDiscarded)
	return nil
}

// Wait blocks until a request with a specified ID is completed (approved or discarded)
func (rs *PendingRequests) Wait(id string, timeout time.Duration) Result {
	request, err := rs.Get(id)
	if err != nil {
		return Result{Error: err}
	}
	for {
		select {
		case rst := <-request.result:
			return rst
		case <-time.After(timeout):
			rs.complete(request, gethcommon.Hash{}, ErrSignReqTimedOut)
		}
	}
}

// Count returns number of currently pending requests
func (rs *PendingRequests) Count() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return len(rs.requests)
}

// Has checks whether a pending request with a given identifier exists in the list
func (rs *PendingRequests) Has(id string) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	_, ok := rs.requests[id]
	return ok
}

// tryLock is used to avoid double-completion of the same request.
// it returns a request instance if it isn't processing yet, returns an error otherwise.
func (rs *PendingRequests) tryLock(id string) (*Request, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if tx, ok := rs.requests[id]; ok {
		if tx.locked {
			return nil, ErrSignReqInProgress
		}
		tx.locked = true
		return tx, nil
	}
	return nil, ErrSignReqNotFound
}

// complete removes the request from the list if there is no error or an error is non-transient
func (rs *PendingRequests) complete(request *Request, hash gethcommon.Hash, err error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	request.locked = false

	go NotifyOnReturn(request, err)

	if err != nil && isTransient(err) {
		return
	}

	delete(rs.requests, request.ID)

	// hash is updated only if err is nil, but transaction is not removed from a queue
	result := Result{Error: err}
	if err == nil {
		result.Hash = hash
	}

	request.result <- result
}
