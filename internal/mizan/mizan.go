package mizan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	balancer "github.com/Mo-Fatah/mizan/internal/pkg/balancer"
	"github.com/Mo-Fatah/mizan/internal/pkg/common"
	"github.com/Mo-Fatah/mizan/internal/pkg/config"
	"github.com/Mo-Fatah/mizan/internal/pkg/health"
	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

type Mizan struct {
	// a general mutex to be used for locking operations on Mizan
	mizanLock *sync.Mutex
	// The reader from which the config is loaded
	configPath string
	// The configuration loaded from the config file
	// TODO (Mo-Fatah): Should add hot reload for config
	config *config.Config
	// Servers is a map of service matcher to a list of servers/replicas
	serversMap map[string]balancer.Balancer
	// Ports to which Mizan will listen on
	ports []int
	// The channel through which Mizan will receive signals to shutdown
	shutdownCh chan struct{}
	// The channel through which Mizan will receive signals to reload config
	reloadCh chan struct{}

	maxConnections uint32

	connections uint32
}

func NewMizan(configPath string) *Mizan {
	shutdownCh := make(chan struct{}, 1)
	reloadCh := make(chan struct{}, 1)
	conf, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Error while loading config: %s", err)
	}

	var ports []int
	if conf.Ports == nil || len(conf.Ports) == 0 {
		ports = []int{433}
	} else {
		ports = conf.Ports
	}

	return &Mizan{
		configPath:     configPath,
		config:         conf,
		ports:          ports,
		shutdownCh:     shutdownCh,
		reloadCh:       reloadCh,
		mizanLock:      &sync.Mutex{},
		maxConnections: conf.MaxConnections,
		connections:    0,
	}
}

// Start starts:
// 1. The config watcher
// 3. The health checker for each service
// 2. The listening servers
func (m *Mizan) Start() {
	if err := m.cfgController(); err != nil {
		log.Fatalf("Error while building servers map: %s", err)
	}

	log.Info("Starting Config Watcher")
	go m.cfgWatcher()

	wg := &sync.WaitGroup{}
	for _, port := range m.ports {
		wg.Add(1)
		go m.startHttpServer(port, wg)
	}
	wg.Wait()
}

// cfgController is responsible for:
// 1. Loading the configs
// 2. Updating the config field in Mizan
// 3. Building the servers map
// 4. Starting the health checker for each service
func (m *Mizan) cfgController() error {

	newConfig, err := config.LoadConfig(m.configPath)
	if err != nil {
		log.Errorf("Error while loading config: %s", err)
		return err
	}
	// If this the first time the config is loaded then we should skip shutting down the health checker
	// otherwise, we need to shutdown the health checkers of the old services
	if m.serversMap != nil {
		for _, service := range m.serversMap {
			service.HealthChecker().ShutDown()
		}
	}

	newServersMap := buildServersMap(newConfig)

	m.mizanLock.Lock()
	m.config = newConfig
	m.serversMap = newServersMap
	m.mizanLock.Unlock()

	// Start health checker
	for _, serviceBalancer := range newServersMap {
		go serviceBalancer.HealthChecker().Start()
	}
	return nil
}

func (m *Mizan) cfgWatcher() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	watcher.Add(m.configPath)
outer:
	for {
		start := time.Now()
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				log.Error("Error while watching config file")
				continue
			}
			if event.Has(fsnotify.Write) {
				// A signle write event can produce multiple write signals
				// This is a hack to avoid double reloads
				// TODO (Mo-Fatah): Find a better way to deduplicate write events
				if time.Since(start) < 100*time.Microsecond {
					continue
				}
				log.Info("Config file has been modified. Reloading config")
				go m.cfgController()
			}

			if event.Has(fsnotify.Remove) {
				log.Error("The config file has been removed. Shutting down Config Watcher")
				break outer
			}
		case err := <-watcher.Errors:
			log.Errorf("Error while watching config file: %s", err)
		}
	}
}

func (m *Mizan) incrementConnections() {
	atomic.AddUint32(&m.connections, 1)
}

func (m *Mizan) decrementConnections() {
	// TODO (Mo-Fatah): There a chance of underflow here. Investigate
	if connections := atomic.LoadUint32(&m.connections); connections > 0 {
		atomic.AddUint32(&m.connections, ^uint32(0))
	}
}

func buildServersMap(conf *config.Config) map[string]balancer.Balancer {
	serversMap := make(map[string]balancer.Balancer)
	for _, service := range conf.Services {
		servers := make([]*common.Server, 0)
		for _, replica := range service.Replicas {
			server := common.NewServer(replica, service.Name)
			servers = append(servers, server)
		}
		serversMap[service.Matcher] = newBalancer(servers, conf.Strategy)
		serversMap[service.Matcher].SetHealthChecker(health.NewHealthChecker(servers, service.Name))
	}
	return serversMap
}

func newBalancer(servers []*common.Server, strategy string) balancer.Balancer {
	switch strings.ToLower(strategy) {
	case "rr":
		return balancer.NewRR(servers)
	case "wrr":
		return balancer.NewWRR(servers)
	default:
		return balancer.NewRR(servers)
	}
}

func (m *Mizan) IsReady() bool {
	for _, port := range m.ports {
		_, err := net.Dial("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			return false
		}
	}
	return true
}

func (m *Mizan) startHttpServer(port int, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Info("Starting http server on port ", port)
	// Timeouts are set to avoid Slowloris attacks. Values are subjectively chosen.
	// see: https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
	server := http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      m,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		// Wait for shutdown signal
		<-m.shutdownCh
		if err := server.Shutdown(context.TODO()); err != nil {
			log.Error(err)
		}
		log.Info("Shutting down server on port ", port)
		// Send shutdown complete signal
		m.shutdownCh <- struct{}{}
	}()

	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error(err)
	}
}

func (m *Mizan) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m.connections >= m.config.MaxConnections {
		w.WriteHeader(http.StatusServiceUnavailable)
		log.Error("Max connections reached")
		return
	}

	m.incrementConnections()
	defer m.decrementConnections()

	service := r.URL.Path
	log.Infof("Request received from address %s to service %s", r.RemoteAddr, service)
	// After the next line being executed, the services map may change due to hot config changes
	// This will lead to us serving a request to a service that may not be in the list or the belonging replicas have changed
	// TODO (Mo-Fatah): Investigate this issue
	balancer, err := m.findService(service)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Error(err)
		return
	}

	server, err := balancer.Next()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Errorf("All servers are down for service %s", service)
		return
	}

	log.Infof("Proxying request to %s", server.GetUrl().String())
	server.Proxy(w, r)
}

func (m *Mizan) findService(path string) (balancer.Balancer, error) {
	if _, ok := m.serversMap[path]; !ok {
		return nil, fmt.Errorf("couldn't find path %s", path)
	}
	return m.serversMap[path], nil
}

func (m *Mizan) ShutDown() bool {
	// Send shutdown signal to all health checkers
	for _, serviceBalancer := range m.serversMap {
		serviceBalancer.HealthChecker().ShutDown()
	}

	// Send shutdown signal to all servers
	for range m.ports {
		// Send shutdown signal
		m.shutdownCh <- struct{}{}
		// Wait for shutdown to complete
		<-m.shutdownCh
	}

	log.Info("All servers are shutdown")
	return true
}
