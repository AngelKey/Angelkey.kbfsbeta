package libkbfs

import (
	"sync"
)

// Service names used in ConnectionStatus.
const (
	KeybaseServiceName = "keybase-service"
	MDServiceName      = "md-server"
)

type errDisconnected struct{}

func (errDisconnected) Error() string { return "Disconnected" }

type kbfsCurrentStatus struct {
	lock            sync.Mutex
	failingServices map[string]error
	invalidateChan  chan StatusUpdate
}

// Init inits the kbfsCurrentStatus.
func (kcs *kbfsCurrentStatus) Init() {
	kcs.failingServices = map[string]error{}
	kcs.invalidateChan = make(chan StatusUpdate)
}

// CurrentStatus returns a copy of the current status.
func (kcs *kbfsCurrentStatus) CurrentStatus() (map[string]error, chan StatusUpdate) {
	kcs.lock.Lock()
	defer kcs.lock.Unlock()

	res := map[string]error{}
	for k, v := range kcs.failingServices {
		res[k] = v
	}
	return res, kcs.invalidateChan
}

// PushConnectionStatusChange pushes a change to the connection status of one of the services.
func (kcs *kbfsCurrentStatus) PushConnectionStatusChange(service string, err error) {
	kcs.lock.Lock()
	defer kcs.lock.Unlock()

	if err != nil {
		kcs.failingServices[service] = err
	} else {
		// Potentially exit early if nothing changes.
		_, exist := kcs.failingServices[service]
		if !exist {
			return
		}
		delete(kcs.failingServices, service)
	}

	close(kcs.invalidateChan)
	kcs.invalidateChan = make(chan StatusUpdate)
}
