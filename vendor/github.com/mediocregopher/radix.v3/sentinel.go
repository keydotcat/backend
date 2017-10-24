package radix

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// Sentinel is a Client which, in the background, connects to an available
// sentinel node and handles all of the following:
//
// * Creates a Pool to the current master instance, as advertised by the
// sentinel
//
// * Listens for events indicating the master has changed, and automatically
// creates a new Client to the new master
//
// * Keeps track of other sentinels in the cluster, and uses them if the
// currently connected one becomes unreachable
//
type Sentinel struct {
	initAddrs []string

	// we read lock when calling methods on cl, and normal lock when swapping
	// the value of cl, clAddr, and addrs
	l      sync.RWMutex
	cl     Client
	clAddr string
	addrs  map[string]bool // the known sentinel addresses

	name string
	dfn  ConnFunc // the function used to dial sentinel instances
	cfn  ClientFunc

	// We use a persistent PubSubConn here, so we don't need to do much after
	// initialization. The pconn is only really kept around for closing
	pconn   PubSubConn
	pconnCh chan PubSubMessage

	// Any errors encountered internally will be written to this channel. If
	// nothing is reading the channel the errors will be dropped. The channel
	// will be closed when the Close is called.
	ErrCh chan error

	closeCh   chan bool
	closeWG   sync.WaitGroup
	closeOnce sync.Once

	// only used by tests to ensure certain actions have happened before
	// continuing on during the test
	testEventCh chan string
}

// NewSentinel creates and returns a *Sentinel. connFn may be nil, but if given
// can specify a custom ConnFunc to use when connecting to sentinels. clientFn
// may be nil, but if given can specify a custom ClientFunc to use when creating
// a client to the master instance.
func NewSentinel(masterName string, sentinelAddrs []string, connFn ConnFunc, clientFn ClientFunc) (*Sentinel, error) {
	if connFn == nil {
		connFn = func(net, addr string) (Conn, error) {
			return DialTimeout(net, addr, 5*time.Second)
		}
	}
	if clientFn == nil {
		clientFn = DefaultClientFunc
	}

	addrs := map[string]bool{}
	for _, addr := range sentinelAddrs {
		addrs[addr] = true
	}

	sc := &Sentinel{
		initAddrs:   sentinelAddrs,
		name:        masterName,
		addrs:       addrs,
		dfn:         connFn,
		cfn:         clientFn,
		pconnCh:     make(chan PubSubMessage),
		ErrCh:       make(chan error, 1),
		closeCh:     make(chan bool),
		testEventCh: make(chan string, 1),
	}

	// first thing is to retrieve the state and create a pool using the first
	// connectable connection. This connection is only used during
	// initialization, it gets closed right after
	{
		conn, err := sc.dial()
		if err != nil {
			return nil, err
		}
		defer conn.Close()

		if err := sc.ensureSentinelAddrs(conn); err != nil {
			return nil, err
		} else if err := sc.ensureMaster(conn); err != nil {
			return nil, err
		}
	}

	// because we're using persistent these can't _really_ fail
	sc.pconn = PersistentPubSub("", "", func(_, _ string) (Conn, error) {
		return sc.dial()
	})
	sc.pconn.Subscribe(sc.pconnCh, "switch-master")

	sc.closeWG.Add(2)
	go sc.spin()
	go sc.pubsubSpin()
	return sc, nil
}

func (sc *Sentinel) err(err error) {
	select {
	case sc.ErrCh <- err:
	default:
	}
}

func (sc *Sentinel) testEvent(event string) {
	select {
	case sc.testEventCh <- event:
	default:
	}
}

func (sc *Sentinel) dial() (Conn, error) {
	sc.l.RLock()
	defer sc.l.RUnlock()

	var conn Conn
	var err error
	for addr := range sc.addrs {
		conn, err = sc.dfn("tcp", addr)
		if err == nil {
			return conn, nil
		}
	}

	// try the initAddrs as a last ditch, but don't return their error if this
	// doesn't work
	for _, addr := range sc.initAddrs {
		if conn, err := sc.dfn("tcp", addr); err == nil {
			return conn, nil
		}
	}

	return nil, err
}

// Do implements the method for the Client interface. It will pass the given
// action on to the current master.
//
// NOTE it's possible that in between Do being called and the Action being
// actually carried out that there could be a failover event. In that case, the
// Action will likely fail and return an error.
func (sc *Sentinel) Do(a Action) error {
	sc.l.RLock()
	defer sc.l.RUnlock()
	return sc.cl.Do(a)
}

// Close implements the method for the Client interface.
func (sc *Sentinel) Close() error {
	sc.l.Lock()
	defer sc.l.Unlock()
	closeErr := errClientClosed
	sc.closeOnce.Do(func() {
		close(sc.closeCh)
		sc.closeWG.Wait()
		closeErr = sc.cl.Close()
	})
	return closeErr
}

// given a connection to a sentinel, ensures that the pool currently being held
// agrees with what the sentinel thinks it should be
func (sc *Sentinel) ensureMaster(conn Conn) error {
	sc.l.RLock()
	lastAddr := sc.clAddr
	sc.l.RUnlock()

	var m map[string]string
	err := conn.Do(Cmd(&m, "SENTINEL", "MASTER", sc.name))
	if err != nil {
		return err
	} else if m["ip"] == "" || m["port"] == "" {
		return errors.New("malformed SENTINEL MASTER response")
	}
	newAddr := m["ip"] + ":" + m["port"]
	if newAddr == lastAddr {
		return nil
	}
	return sc.setMaster(newAddr)
}

func (sc *Sentinel) setMaster(newAddr string) error {
	newPool, err := sc.cfn("tcp", newAddr)
	if err != nil {
		return err
	}

	sc.l.Lock()
	if sc.cl != nil {
		sc.cl.Close()
	}
	sc.cl = newPool
	sc.clAddr = newAddr
	sc.l.Unlock()

	return nil
}

// annoyingly the SENTINEL SENTINELS <name> command doesn't return _this_
// sentinel instance, only the others it knows about for that master
func (sc *Sentinel) ensureSentinelAddrs(conn Conn) error {
	var mm []map[string]string
	err := conn.Do(Cmd(&mm, "SENTINEL", "SENTINELS", sc.name))
	if err != nil {
		return err
	}

	addrs := map[string]bool{conn.NetConn().RemoteAddr().String(): true}
	for _, m := range mm {
		addrs[m["ip"]+":"+m["port"]] = true
	}

	sc.l.Lock()
	sc.addrs = addrs
	sc.l.Unlock()
	return nil
}

func (sc *Sentinel) spin() {
	defer sc.closeWG.Done()
	for {
		if err := sc.innerSpin(); err != nil {
			sc.err(err)
			// sleep a second so we don't end up in a tight loop
			time.Sleep(1 * time.Second)
		}
		// This also gets checked within innerSpin to short-circuit that, but
		// we also must check in here to short-circuit this
		select {
		case <-sc.closeCh:
			return
		default:
		}
	}
}

// makes connection to an address in sc.addrs and handles
// the sentinel until that connection goes bad.
//
// Things this handles:
// * Listening for switch-master events (from pconn, which has reconnect logic
//   external to this package)
// * Periodically re-ensuring that the list of sentinel addresses is up-to-date
// * Periodically re-checking the current master, in case the switch-master was
//   missed somehow
func (sc *Sentinel) innerSpin() error {
	conn, err := sc.dial()
	if err != nil {
		return err
	}
	defer conn.Close()

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if err := sc.ensureSentinelAddrs(conn); err != nil {
				return err
			}
			if err := sc.ensureMaster(conn); err != nil {
				return err
			}
			sc.pconn.Ping()
		case <-sc.closeCh:
			return nil
		}
	}
}

func (sc *Sentinel) pubsubSpin() {
	defer sc.closeWG.Done()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case msg := <-sc.pconnCh:
			parts := strings.Split(string(msg.Message), " ")
			if len(parts) < 5 || parts[0] != sc.name || msg.Channel != "switch-master" {
				continue
			}
			newAddr := parts[3] + ":" + parts[4]
			if err := sc.setMaster(newAddr); err != nil {
				sc.err(err)
			}
			sc.testEvent("switch-master completed")
		case <-tick.C:
			sc.pconn.Ping()
		case <-sc.closeCh:
			sc.pconn.Close()
			return
		}
	}
}
