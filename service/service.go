package service

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log/syslog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	logrus_syslog "github.com/Sirupsen/logrus/hooks/syslog"
	logrus_logstash "github.com/bshuster-repo/logrus-logstash-hook"
	etcd "github.com/coreos/etcd/client"
	"github.com/gorilla/mux"
	"github.com/mailgun/manners"
	"github.com/mailgun/metrics"
	"github.com/vulcand/vulcand/api"
	"github.com/vulcand/vulcand/engine"
	"github.com/vulcand/vulcand/engine/etcdv2ng"
	"github.com/vulcand/vulcand/engine/etcdv3ng"
	"github.com/vulcand/vulcand/plugin"
	"github.com/vulcand/vulcand/proxy"
	"github.com/vulcand/vulcand/secret"
	"github.com/vulcand/vulcand/stapler"
	"github.com/vulcand/vulcand/supervisor"
)

type ControlCode int

const (
	ControlCodeGracefulShutdown ControlCode = iota
	ControlCodeImmediateShutdown
	ControlCodeForkChild
)

func waitForSignals() chan ControlCode {
	sigC := make(chan os.Signal, 1024)
	signal.Notify(sigC, os.Interrupt, os.Kill, syscall.SIGTERM, syscall.SIGUSR2)
	controlC := make(chan ControlCode, 1024)

	go func() {
		for {
			signal := <-sigC
			log.Infof("Got signal '%s'", signal)

			switch signal {
			case syscall.SIGTERM, syscall.SIGINT:
				controlC <- ControlCodeGracefulShutdown
			case syscall.SIGKILL:
				controlC <- ControlCodeImmediateShutdown
			case syscall.SIGUSR2:
				controlC <- ControlCodeForkChild
			default:
				log.Infof("Ignoring signal '%s'", signal)
			}
		}
	}()

	return controlC
}

func Run(registry *plugin.Registry) error {
	options, err := ParseCommandLine()
	if err != nil {
		return fmt.Errorf("failed to parse command line: %s", err)
	}
	if options.MemProfileRate > 0 {
		runtime.MemProfileRate = options.MemProfileRate
	}

	service := NewService(options, registry)
	if err := service.Start(waitForSignals()); err != nil {
		log.Errorf("Failed to start service: %v", err)
		return fmt.Errorf("service start failure: %s", err)
	} else {
		log.Infof("Service exited gracefully")
	}
	return nil
}

type Service struct {
	client        etcd.Client
	options       Options
	registry      *plugin.Registry
	errorC        chan error
	supervisor    *supervisor.Supervisor
	metricsClient metrics.Client
	apiServer     *manners.GracefulServer
	ng            engine.Engine
	stapler       stapler.Stapler
}

func NewService(options Options, registry *plugin.Registry) *Service {
	return &Service{
		registry: registry,
		options:  options,
		errorC:   make(chan error),
	}
}

func (s *Service) Start(controlC chan ControlCode) error {
	// if .LogFormatter is set, it'll be used in log.SetFormatter() and .Log will be ignored.
	if s.options.LogFormatter != nil {
		log.SetFormatter(s.options.LogFormatter)
	} else {
		switch s.options.Log {
		case "console":
			{
				log.SetFormatter(&log.TextFormatter{})
			}
		case "json":
			{
				log.SetFormatter(&log.JSONFormatter{})
			}
		case "syslog":
			{
				hook, err := logrus_syslog.NewSyslogHook("", "", syslog.LOG_INFO, "")
				if err == nil {
					log.SetFormatter(&log.TextFormatter{DisableColors: true})
					log.AddHook(hook)
				} else {
					setFallbackLogFormatter(s.options)
				}
			}
		case "logstash":
			{
				log.SetFormatter(&logrus_logstash.LogstashFormatter{Type: "logs"})
			}
		default:
			setFallbackLogFormatter(s.options)
		}
	}
	log.SetOutput(os.Stdout)
	log.SetLevel(s.options.LogSeverity.S)

	log.Infof("Service starts with options: %#v", s.options)

	if s.options.PidPath != "" {
		ioutil.WriteFile(s.options.PidPath, []byte(fmt.Sprint(os.Getpid())), 0644)
	}

	if s.options.MetricsClient != nil {
		s.metricsClient = s.options.MetricsClient
	} else if s.options.StatsdAddr != "" {
		var err error
		s.metricsClient, err = metrics.NewWithOptions(s.options.StatsdAddr, s.options.StatsdPrefix, metrics.Options{UseBuffering: true})
		if err != nil {
			return err
		}
	}

	apiFile, muxFiles, err := s.getFiles()
	if err != nil {
		return err
	}

	if err := s.newEngine(); err != nil {
		return err
	}

	s.stapler = stapler.New()
	s.supervisor = supervisor.New(s.newProxy, s.ng, supervisor.Options{Files: muxFiles})

	// Tells configurator to perform initial proxy configuration and start watching changes
	if err := s.supervisor.Start(); err != nil {
		return err
	}

	go func() {
		s.errorC <- s.startApi(apiFile)
	}()

	if s.metricsClient != nil {
		go s.reportSystemMetrics()
	}

	sigC := make(chan os.Signal, 1024)
	signal.Notify(sigC, syscall.SIGCHLD)

	// Block until a signal is received or we got an error
	for {
		select {
		case signal := <-sigC:
			switch signal {
			case syscall.SIGCHLD:
				log.Warningf("Child exited, got '%s', collecting status", signal)
				var wait syscall.WaitStatus
				syscall.Wait4(-1, &wait, syscall.WNOHANG, nil)
				log.Warningf("Collected exit status from child")
			default:
				log.Infof("Ignoring signal '%s'", signal)
			}

		case controlCode := <-controlC:
			switch controlCode {
			case ControlCodeGracefulShutdown:
				log.Info("Got graceful shutdown control code")
				s.supervisor.Stop()
				log.Infof("All servers stopped")
				return nil
			case ControlCodeImmediateShutdown:
				log.Info("Got immediate shutdown control code")
				s.supervisor.Stop()
				return nil
			case ControlCodeForkChild:
				log.Infof("Got fork child control code")
				if err := s.startChild(); err != nil {
					log.Infof("Failed to start self: %s", err)
				} else {
					log.Infof("Successfully started self")
				}
			}

		case err := <-s.errorC:
			log.Infof("Got request to shutdown with error: %s", err)
			return err
		}
	}
}

func (s *Service) getFiles() (*proxy.FileDescriptor, []*proxy.FileDescriptor, error) {
	// These files may be passed in by the parent process
	filesString := os.Getenv(vulcandFilesKey)
	if filesString == "" {
		return nil, nil, nil
	}

	files, err := filesFromString(filesString)
	if err != nil {
		return nil, nil, fmt.Errorf("child failed to start: failed to read files from string, error %s", err)
	}

	if len(files) != 0 {
		log.Infof("I am a child that has been passed files: %s", files)
	}

	return s.splitFiles(files)
}

func (s *Service) splitFiles(files []*proxy.FileDescriptor) (*proxy.FileDescriptor, []*proxy.FileDescriptor, error) {
	apiAddr := fmt.Sprintf("%s:%d", s.options.ApiInterface, s.options.ApiPort)
	for i, f := range files {
		if f.Address.Address == apiAddr {
			return files[i], append(files[:i], files[i+1:]...), nil
		}
	}
	return nil, nil, fmt.Errorf("API address %s not found in %s", apiAddr, files)
}

func (s *Service) startChild() error {
	log.Infof("Starting child")
	path, err := execPath()
	if err != nil {
		return err
	}

	wd, err := os.Getwd()
	if nil != err {
		return err
	}

	// Get socket files currently in use by the underlying http server controlled by supervisor
	extraFiles, err := s.supervisor.GetFiles()
	if err != nil {
		return err
	}

	apiFile, err := s.GetAPIFile()
	if err != nil {
		return err
	}

	extraFiles = append(extraFiles, apiFile)

	// These files will be passed to the child process
	files := []*os.File{os.Stdin, os.Stdout, os.Stderr}
	for _, f := range extraFiles {
		files = append(files, f.File)
	}

	// Serialize files to JSON string representation
	vals, err := filesToString(extraFiles)
	if err != nil {
		return err
	}

	log.Infof("Passing %s to child", vals)
	os.Setenv(vulcandFilesKey, vals)

	p, err := os.StartProcess(path, os.Args, &os.ProcAttr{
		Dir:   wd,
		Env:   os.Environ(),
		Files: files,
		Sys:   &syscall.SysProcAttr{},
	})

	if err != nil {
		return err
	}

	log.Infof("Started new child pid=%d binary=%s", p.Pid, path)
	return nil
}

func (s *Service) GetAPIFile() (*proxy.FileDescriptor, error) {
	file, err := s.apiServer.GetFile()
	if err != nil {
		return nil, err
	}
	a := engine.Address{
		Network: "tcp",
		Address: fmt.Sprintf("%s:%d", s.options.ApiInterface, s.options.ApiPort),
	}
	return &proxy.FileDescriptor{File: file, Address: a}, nil
}

func (s *Service) newBox() (*secret.Box, error) {
	if s.options.SealKey == "" {
		return nil, nil
	}
	key, err := secret.KeyFromString(s.options.SealKey)
	if err != nil {
		return nil, err
	}
	return secret.NewBox(key)
}

func (s *Service) newEngine() error {
	box, err := s.newBox()
	if err != nil {
		return err
	}
	var ng engine.Engine

	if s.options.EtcdApiVersion == 3 {
		ng, err = etcdv3ng.New(
			s.options.EtcdNodes,
			s.options.EtcdKey,
			s.registry,
			etcdv3ng.Options{
				EtcdCaFile:              s.options.EtcdCaFile,
				EtcdCertFile:            s.options.EtcdCertFile,
				EtcdKeyFile:             s.options.EtcdKeyFile,
				EtcdConsistency:         s.options.EtcdConsistency,
				EtcdSyncIntervalSeconds: s.options.EtcdSyncIntervalSeconds,
				Box: box,
			})
	} else {
		ng, err = etcdv2ng.New(
			s.options.EtcdNodes,
			s.options.EtcdKey,
			s.registry,
			etcdv2ng.Options{
				EtcdCaFile:              s.options.EtcdCaFile,
				EtcdCertFile:            s.options.EtcdCertFile,
				EtcdKeyFile:             s.options.EtcdKeyFile,
				EtcdConsistency:         s.options.EtcdConsistency,
				EtcdSyncIntervalSeconds: s.options.EtcdSyncIntervalSeconds,
				Box: box,
			})
	}
	if err != nil {
		return err
	}
	s.ng = ng
	return err
}

func (s *Service) reportSystemMetrics() {
	defer func() {
		if r := recover(); r != nil {
			log.Infof("Recovered in reportSystemMetrics", r)
		}
	}()
	for {
		s.metricsClient.ReportRuntimeMetrics("sys", 1.0)
		// we have 256 time buckets for gc stats, GC is being executed every 4ms on average
		// so we have 256 * 4 = 1024 around one second to report it. To play safe, let's report every 300ms
		time.Sleep(300 * time.Millisecond)
	}
}

func (s *Service) newProxy(id int) (proxy.Proxy, error) {
	return proxy.New(id, s.stapler, proxy.Options{
		MetricsClient:      s.metricsClient,
		DialTimeout:        s.options.EndpointDialTimeout,
		ReadTimeout:        s.options.ServerReadTimeout,
		WriteTimeout:       s.options.ServerWriteTimeout,
		MaxHeaderBytes:     s.options.ServerMaxHeaderBytes,
		DefaultListener:    constructDefaultListener(s.options),
		NotFoundMiddleware: s.registry.GetNotFoundMiddleware(),
		Router:             s.registry.GetRouter(),
		IncomingConnectionTracker: s.registry.GetIncomingConnectionTracker(),
		OutgoingConnectionTracker: s.registry.GetOutgoingConnectionTracker(),
	})
}

func (s *Service) startApi(file *proxy.FileDescriptor) error {
	addr := fmt.Sprintf("%s:%d", s.options.ApiInterface, s.options.ApiPort)

	router := mux.NewRouter()
	api.InitProxyController(s.ng, s.supervisor, router)

	server := &http.Server{
		Addr:           addr,
		Handler:        router,
		ReadTimeout:    s.options.ServerReadTimeout,
		WriteTimeout:   s.options.ServerWriteTimeout,
		MaxHeaderBytes: 1 << 20,
	}

	var listener net.Listener
	if file != nil {
		var err error
		listener, err = file.ToListener()
		if err != nil {
			return err
		}
	}

	s.apiServer = manners.NewWithOptions(manners.Options{Server: server, Listener: listener})
	return s.apiServer.ListenAndServe()
}

func constructDefaultListener(options Options) *engine.Listener {
	if options.DefaultListener {
		return &engine.Listener{
			Id:       "DefaultListener",
			Protocol: "http",
			Address: engine.Address{
				Network: "tcp",
				Address: fmt.Sprintf("%s:%d", options.Interface, options.Port),
			},
		}
	}
	return nil
}

func execPath() (string, error) {
	name, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	if _, err = os.Stat(name); nil != err {
		return "", err
	}
	return name, err
}

type fileDescriptor struct {
	Address  engine.Address
	FileFD   int
	FileName string
}

// filesToString serializes file descriptors as well as accompanying information (like socket host and port)
func filesToString(files []*proxy.FileDescriptor) (string, error) {
	out := make([]fileDescriptor, len(files))
	for i, f := range files {
		out[i] = fileDescriptor{
			// Once files will be passed to the child process and their FDs will change.
			// The first three passed files are stdin, stdout and stderr, every next file will have the index + 3
			// That's why we rearrange the FDs for child processes to get the correct file descriptors.
			FileFD:   i + 3,
			FileName: f.File.Name(),
			Address:  f.Address,
		}
	}
	bytes, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// filesFromString de-serializes the file descriptors and turns them in the os.Files
func filesFromString(in string) ([]*proxy.FileDescriptor, error) {
	var out []fileDescriptor
	if err := json.Unmarshal([]byte(in), &out); err != nil {
		return nil, err
	}
	files := make([]*proxy.FileDescriptor, len(out))
	for i, o := range out {
		files[i] = &proxy.FileDescriptor{
			File:    os.NewFile(uintptr(o.FileFD), o.FileName),
			Address: o.Address,
		}
	}
	return files, nil
}

func setFallbackLogFormatter(options Options) {
	log.Warnf("Invalid logger ty pe %v, and no LogFormatter %v, fallback to default.", options.Log, options.LogFormatter)
	log.SetFormatter(&log.TextFormatter{})
}

const vulcandFilesKey = "VULCAND_FILES_KEY"
