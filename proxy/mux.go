package proxy

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/mailgun/metrics"
	"github.com/mailgun/timetools"
	"github.com/pkg/errors"
	"github.com/vulcand/oxy/forward"
	"github.com/vulcand/route"
	"github.com/vulcand/vulcand/conntracker"
	"github.com/vulcand/vulcand/engine"
	"github.com/vulcand/vulcand/router"
	"github.com/vulcand/vulcand/stapler"
)

// mux is capable of listening on multiple interfaces, graceful shutdowns and updating TLS certificates
type mux struct {
	// Debugging id
	id int

	// Each listener address has a server associated with it
	servers map[engine.ListenerKey]*srv

	backends map[engine.BackendKey]*backend

	frontends map[engine.FrontendKey]*frontend

	hosts map[engine.HostKey]engine.Host

	// Options hold parameters that are used to initialize http servers
	options Options

	// Wait group for graceful shutdown
	wg *sync.WaitGroup

	// Read write mutex for serialized operations
	mtx *sync.RWMutex

	// Router will be shared between multiple listeners
	router router.Router

	// Current server stats
	state muxState

	// Connection watcher
	incomingConnTracker conntracker.ConnectionTracker

	// Connection watcher
	outgoingConnTracker forward.UrlForwardingStateListener

	// stopC used for global broadcast to all proxy systems that it's closed
	stopC chan struct{}

	// OCSP staple cache and responder
	stapler stapler.Stapler

	// Unsubscribe from staple updates
	stapleUpdatesC chan *stapler.StapleUpdated
}

func (m *mux) String() string {
	return fmt.Sprintf("mux_%d", m.id)
}

func New(id int, st stapler.Stapler, o Options) (*mux, error) {
	o = setDefaults(o)
	m := &mux{
		id:  id,
		wg:  &sync.WaitGroup{},
		mtx: &sync.RWMutex{},

		options: o,

		router:              o.Router,
		incomingConnTracker: o.IncomingConnectionTracker,
		outgoingConnTracker: o.OutgoingConnectionTracker,

		servers:   make(map[engine.ListenerKey]*srv),
		backends:  make(map[engine.BackendKey]*backend),
		frontends: make(map[engine.FrontendKey]*frontend),
		hosts:     make(map[engine.HostKey]engine.Host),

		stapleUpdatesC: make(chan *stapler.StapleUpdated),
		stopC:          make(chan struct{}),
		stapler:        st,
	}

	m.router.SetNotFound(&DefaultNotFound{})
	if o.NotFoundMiddleware != nil {
		if handler, err := o.NotFoundMiddleware.NewHandler(m.router.GetNotFound()); err == nil {
			m.router.SetNotFound(handler)
		}
	}

	if m.options.DefaultListener != nil {
		if err := m.upsertListener(*m.options.DefaultListener); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *mux) Init(ss engine.Snapshot) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	for _, host := range ss.Hosts {
		m.hosts[engine.HostKey{Name: host.Name}] = host
	}

	for _, bes := range ss.BackendSpecs {
		beKey := engine.BackendKey{Id: bes.Backend.Id}
		be, err := newBackend(m, bes.Backend)
		if err != nil {
			return errors.Wrapf(err, "failed to create backend %v", bes.Backend.Id)
		}
		be.servers = make([]engine.Server, len(bes.Servers))
		for i, beSrv := range bes.Servers {
			if _, err := url.ParseRequestURI(beSrv.URL); err != nil {
				return errors.Wrapf(err, "failed to parse %v", beSrv)
			}
			be.servers[i] = beSrv
		}
		m.backends[beKey] = be
	}

	for _, l := range ss.Listeners {
		for _, feSrv := range m.servers {
			if feSrv.listener.Address == l.Address {
				// This only exists to simplify test fixture configuration.
				if feSrv.listener.Id == l.Id {
					continue
				}
				return errors.Errorf("%v conflicts with existing %v", l.Id, feSrv.listener.Id)
			}
		}
		feSrv, err := newSrv(m, l)
		if err != nil {
			return errors.Wrapf(err, "failed to create server %v", l.Id)
		}
		m.servers[engine.ListenerKey{Id: l.Id}] = feSrv
	}

	for _, fes := range ss.FrontendSpecs {
		feKey := engine.FrontendKey{fes.Frontend.Id}
		be, ok := m.backends[engine.BackendKey{Id: fes.Frontend.BackendId}]
		if !ok {
			return errors.Errorf("unknown backend %v in frontend %v",
				fes.Frontend.BackendId, fes.Frontend.Id)
		}
		fe := newFrontend(m, fes.Frontend, be)
		for _, mw := range fes.Middlewares {
			fe.middlewares[engine.MiddlewareKey{FrontendKey: feKey, Id: mw.Id}] = mw
		}

		if err := fe.rebuild(); err != nil {
			return errors.Wrapf(err, "failed to rebuild frontend %v", fes.Frontend.Id)
		}
		be.linkFrontend(feKey, fe)
		m.frontends[feKey] = fe
	}
	return nil
}

func (m *mux) GetFiles() ([]*FileDescriptor, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	fds := []*FileDescriptor{}

	for _, srv := range m.servers {
		fd, err := srv.GetFile()
		if err != nil {
			return nil, err
		}
		if fd != nil {
			fds = append(fds, fd)
		}
	}
	return fds, nil
}

func (m *mux) TakeFiles(files []*FileDescriptor) error {
	log.Infof("%s TakeFiles %s", m, files)

	fMap := make(map[engine.Address]*FileDescriptor, len(files))
	for _, f := range files {
		fMap[f.Address] = f
	}

	m.mtx.Lock()
	defer m.mtx.Unlock()

	for _, srv := range m.servers {

		file, exists := fMap[srv.listener.Address]
		if !exists {
			log.Infof("%s skipping take of files from address %s, has no passed files", m, srv.listener.Address)
			continue
		}
		if err := srv.takeFile(file); err != nil {
			return err
		}
	}

	return nil
}

func (m *mux) Start() error {
	log.Infof("%s start", m)
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.state != stateInit {
		return fmt.Errorf("%s can start only from init state, got %d", m, m.state)
	}

	// Subscribe to staple responses and kick staple updates
	m.stapler.Subscribe(m.stapleUpdatesC, m.stopC)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case <-m.stopC:
				log.Infof("%v stop listening for staple updates", m)
				return
			case e := <-m.stapleUpdatesC:
				m.processStapleUpdate(e)
			}
		}
	}()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case <-m.stopC:
				log.Infof("%v stop emitting metrics", m)
				return
			case <-time.After(time.Second):
				m.emitMetrics()
			}
		}
	}()

	m.state = stateActive
	for _, s := range m.servers {
		if err := s.start(); err != nil {
			return err
		}
	}

	log.Infof("%s started", m)
	return nil
}

func (m *mux) Stop(wait bool) {
	log.Infof("%s Stop(%t)", m, wait)

	m.stopServers()

	if wait {
		log.Infof("%s waiting for the wait group to finish", m)
		m.wg.Wait()
		log.Infof("%s wait group finished", m)
	}
}

func (m *mux) stopServers() {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.state == stateShuttingDown {
		log.Infof("%v is already shutting down", m)
		return
	}

	prevState := m.state
	m.state = stateShuttingDown
	close(m.stopC)

	// init state has no running servers, no need to close them
	if prevState == stateInit {
		return
	}

	for _, s := range m.servers {
		s.shutdown()
	}
}

func (m *mux) UpsertHost(host engine.Host) error {
	log.Infof("%s UpsertHost %s", m, &host)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	m.hosts[engine.HostKey{Name: host.Name}] = host

	for _, s := range m.servers {
		if s.isTLS() {
			s.reload()
		}
	}
	return nil
}

func (m *mux) DeleteHost(hk engine.HostKey) error {
	log.Infof("%s DeleteHost %v", m, &hk)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	host, exists := m.hosts[hk]
	if !exists {
		return &engine.NotFoundError{Message: fmt.Sprintf("%v not found", hk)}
	}

	// delete host from the hosts list
	delete(m.hosts, hk)

	// delete staple from the cache
	m.stapler.DeleteHost(hk)

	if host.Settings.KeyPair == nil {
		return nil
	}

	for _, s := range m.servers {
		s.reload()
	}
	return nil
}

func (m *mux) UpsertListener(l engine.Listener) error {
	log.Infof("%v UpsertListener %v", m, &l)
	m.mtx.Lock()
	defer m.mtx.Unlock()

	return m.upsertListener(l)
}

func (m *mux) DeleteListener(lk engine.ListenerKey) error {
	log.Infof("%v DeleteListener %v", m, &lk)
	m.mtx.Lock()
	defer m.mtx.Unlock()

	s, exists := m.servers[lk]
	if !exists {
		return &engine.NotFoundError{Message: fmt.Sprintf("%v not found", lk)}
	}

	delete(m.servers, lk)
	s.shutdown()
	return nil
}

func (m *mux) upsertListener(l engine.Listener) error {
	lk := engine.ListenerKey{Id: l.Id}
	s, exists := m.servers[lk]
	if exists {
		return s.updateListener(l)
	}

	// Check if there's a listener with the same address
	for _, srv := range m.servers {
		if srv.listener.Address == l.Address {
			return &engine.AlreadyExistsError{Message: fmt.Sprintf("%v conflicts with existing %v", l, srv.listener)}
		}
	}

	var err error
	if s, err = newSrv(m, l); err != nil {
		return err
	}
	m.servers[lk] = s
	// If we are active, start the server immediatelly
	if m.state == stateActive {
		log.Infof("Mux is in active state, starting the HTTP server")
		if err := s.start(); err != nil {
			return err
		}
	}
	return nil
}

func (m *mux) UpsertBackend(b engine.Backend) error {
	log.Infof("%v UpsertBackend %v", m, &b)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	_, err := m.upsertBackend(b)
	return err
}

func (m *mux) upsertBackend(be engine.Backend) (*backend, error) {
	bk := engine.BackendKey{Id: be.Id}
	b, ok := m.backends[bk]
	if ok {
		return b, b.update(be)
	}
	b, err := newBackend(m, be)
	if err != nil {
		return nil, err
	}
	m.backends[bk] = b
	return b, nil
}

func (m *mux) DeleteBackend(bk engine.BackendKey) error {
	log.Infof("%v DeleteBackend %s", m, &bk)

	m.mtx.Lock()
	defer m.mtx.Unlock()
	b, ok := m.backends[bk]
	if !ok {
		return &engine.NotFoundError{Message: fmt.Sprintf("%v not found", bk)}
	}

	//delete backend from being referenced - it is no longer in etcd
	//and future frontend additions to etcd shouldn't see a
	//magical backend just because vulcan is holding a reference to it.
	delete(m.backends, bk)

	if len(b.frontends) != 0 {
		return fmt.Errorf("%v is used by frontends: %v", b, b.frontends)
	}

	b.Close()
	return nil
}

func (m *mux) UpsertFrontend(f engine.Frontend) error {
	log.Infof("%v UpsertFrontend %v", m, &f)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	_, err := m.upsertFrontend(f)
	return err
}

func (m *mux) upsertFrontend(fe engine.Frontend) (*frontend, error) {
	bk := engine.BackendKey{Id: fe.BackendId}
	b, ok := m.backends[bk]
	if !ok {
		return nil, &engine.NotFoundError{Message: fmt.Sprintf("%v not found", bk)}
	}
	fk := engine.FrontendKey{Id: fe.Id}
	f, ok := m.frontends[fk]
	if ok {
		return f, f.update(fe, b)
	}

	f = newFrontend(m, fe, b)
	if err := f.rebuild(); err != nil {
		return nil, err
	}
	b.linkFrontend(fk, f)
	m.frontends[fk] = f
	return f, nil
}

func (m *mux) DeleteFrontend(fk engine.FrontendKey) error {
	log.Infof("%v DeleteFrontend %v", m, &fk)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	return m.deleteFrontend(fk)
}

func (m *mux) deleteFrontend(fk engine.FrontendKey) error {
	f, ok := m.frontends[fk]
	if !ok {
		return &engine.NotFoundError{Message: fmt.Sprintf("%v not found", fk)}
	}
	if err := f.remove(); err != nil {
		return err
	}
	delete(m.frontends, fk)
	return nil
}

func (m *mux) UpsertMiddleware(fk engine.FrontendKey, mi engine.Middleware) error {
	log.Infof("%v UpsertMiddleware %v, %v", m, &fk, &mi)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	return m.upsertMiddleware(fk, mi)
}

func (m *mux) upsertMiddleware(fk engine.FrontendKey, mi engine.Middleware) error {
	f, ok := m.frontends[fk]
	if !ok {
		return &engine.NotFoundError{Message: fmt.Sprintf("%v not found", fk)}
	}
	return f.upsertMiddleware(fk, mi)
}

func (m *mux) DeleteMiddleware(mk engine.MiddlewareKey) error {
	log.Infof("%v DeleteMiddleware(%v %v)", m, &mk)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	f, ok := m.frontends[mk.FrontendKey]
	if !ok {
		return &engine.NotFoundError{Message: fmt.Sprintf("%v not found", mk)}
	}

	return f.deleteMiddleware(mk)
}

func (m *mux) UpsertServer(bk engine.BackendKey, srv engine.Server) error {
	log.Infof("%v UpsertServer %v %v", m, &bk, &srv)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	if _, err := url.ParseRequestURI(srv.URL); err != nil {
		return fmt.Errorf("failed to parse %v, error: %v", srv, err)
	}

	b, ok := m.backends[bk]
	if !ok {
		var err error
		if b, err = m.upsertBackend(engine.Backend{Id: bk.Id, Type: engine.HTTP, Settings: engine.HTTPBackendSettings{}}); err != nil {
			return err
		}
	}

	return b.upsertServer(srv)
}

func (m *mux) DeleteServer(sk engine.ServerKey) error {
	log.Infof("%v DeleteServer %v", m, &sk)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	b, ok := m.backends[sk.BackendKey]
	if !ok {
		return &engine.NotFoundError{Message: fmt.Sprintf("%v not found", sk.BackendKey)}
	}

	return b.deleteServer(sk)
}

func (m *mux) transportSettings(b engine.Backend) (*engine.TransportSettings, error) {
	s, err := b.TransportSettings()
	if err != nil {
		return nil, err
	}
	// Apply global defaults if options are not set
	if s.Timeouts.Dial == 0 {
		s.Timeouts.Dial = m.options.DialTimeout
	}
	if s.Timeouts.Read == 0 {
		s.Timeouts.Read = m.options.ReadTimeout
	}
	return s, nil
}

func (m *mux) processStapleUpdate(e *stapler.StapleUpdated) error {
	log.Infof("%v processStapleUpdate event: %v", m, e)
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if _, ok := m.hosts[e.HostKey]; !ok {
		log.Infof("%v %v from the staple update is not found, skipping", m, e.HostKey)
		return nil
	}

	for _, s := range m.servers {
		if s.isTLS() {
			// each server will ask stapler for the new OCSP response during reload
			s.reload()
		}
	}
	return nil
}

type muxState int

const (
	stateInit         = iota // Server has been created, but does not accept connections yet
	stateActive              // Server is active and accepting connections
	stateShuttingDown        // Server is active, but is draining existing connections and does not accept new connections.
)

func (s muxState) String() string {
	switch s {
	case stateInit:
		return "init"
	case stateActive:
		return "active"
	case stateShuttingDown:
		return "shutting down"
	}
	return "undefined"
}

func setDefaults(o Options) Options {
	if o.MetricsClient == nil {
		o.MetricsClient = metrics.NewNop()
	}
	if o.TimeProvider == nil {
		o.TimeProvider = &timetools.RealTime{}
	}
	if o.Router == nil {
		o.Router = route.NewMux()
	}
	if o.IncomingConnectionTracker == nil {
		o.IncomingConnectionTracker = newDefaultConnTracker()
	}
	return o
}

// NotFound is a generic http.Handler for request
type DefaultNotFound struct {
}

// ServeHTTP returns a simple 404 Not found response
func (*DefaultNotFound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Infof("Not found: %v %v", r.Method, r.URL)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"error":"not found"}`)
}
