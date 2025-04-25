package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	upstreamDnsDefault    = "8.8.8.8:53,4.4.4.4:53"
	listenDnsPortDefault  = "53"
	listenHttpPortDefault = "80"

	ingressMap = make(map[string]string)

	namespaceFlag      *string = nil
	kubeconfigFlag     *string = nil
	upstreamsFlag      *string = nil
	listenDnsPortFlag  *string = nil
	listenHttpPortFlag *string = nil
	debugFlag          *bool   = nil

	upstreamsList []string
	log           *zap.Logger

	dnsUdpClient *dns.Client
	dnsTcpClient *dns.Client
)

func init() {
	dnsUdpClient = &dns.Client{}
	dnsTcpClient = &dns.Client{Net: "tcp"}

	debugFlag = flag.Bool("debug", false, "Enable debug logging")
	if home := homedir.HomeDir(); home != "" {
		d := filepath.Join(home, ".kube", "config")
		kubeconfigFlag = flag.String("kubeconfig", d, "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfigFlag = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	namespaceFlag = flag.String("namespace", "", "(optional) watch ingress objects only in the namespace")
	upstreamsFlag = flag.String("upstreams", upstreamDnsDefault, "(optional) coma-separated [type://]host:port list of upstreams")
	listenDnsPortFlag = flag.String("dns-port", listenDnsPortDefault, "(optional) dns interface port")
	listenHttpPortFlag = flag.String("http-port", listenHttpPortDefault, "(optional) http interface port")
	flag.Parse()

	if *debugFlag {
		log = zap.Must(zap.NewDevelopment())
		log.Info("Starting development logger")
	} else {
		log = zap.Must(zap.NewProduction())
		log.Info("Starting production logger")
	}

	upstreamsList = strings.Split(*upstreamsFlag, ",")
	for i, k := range upstreamsList {
		upstreamsList[i] = strings.TrimSpace(k)
	}
	log.Info("Loaded upstreams", zap.String("upstreams", strings.Join(upstreamsList, ",")))
}

func main() {
	stopChan := make(chan struct{})
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	startIngressInformer(stopChan)
	startDnsServer(stopChan)
	startHttpServer(stopChan)

	<-exitChan
	log.Info("Shutting down...")
	log.Sync()
	close(stopChan)
}

func startHttpServer(quitChan <-chan struct{}) error {
	http.HandleFunc("/", handleHttpRequest)
	srv := http.Server{
		Addr: fmt.Sprintf(":%s", *listenHttpPortFlag),
	}
	go func() {
		log.Info("Starting HTTP server...", zap.String("port", *listenHttpPortFlag))
		if err := srv.ListenAndServe(); err != nil {
			log.Fatal("Failed to start HTTP server", zap.Error(err))
		}
	}()
	go func() {
		<-quitChan
		srv.Shutdown(context.Background())
	}()
	return nil
}

func startDnsServer(quitChan <-chan struct{}) error {
	dns.HandleFunc(".", handleDnsRequest)
	srv := &dns.Server{
		Addr: fmt.Sprintf(":%s", *listenDnsPortFlag),
		Net:  "udp",
	}
	go func() {
		log.Info("Starting DNS server...", zap.String("port", *listenDnsPortFlag))
		if err := srv.ListenAndServe(); err != nil {
			log.Fatal("Failed to start DNS server", zap.Error(err))
		}
	}()
	go func() {
		<-quitChan
		srv.Shutdown()
	}()
	return nil
}

func startIngressInformer(quitChan <-chan struct{}) error {
	clientset, err := getClientset()
	if err != nil {
		return err
	}

	var opts []informers.SharedInformerOption
	if *namespaceFlag != "" {
		opts = append(opts, informers.WithNamespace(*namespaceFlag))
	}

	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0, opts...)
	informer := factory.Networking().V1().Ingresses().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			updateIngressMap(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			updateIngressMap(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			deleteIngressMap(obj)
		},
	})

	go func() {
		log.Info("Starting Ingress informer...")
		informer.Run(quitChan)
	}()

	if !cache.WaitForCacheSync(quitChan, informer.HasSynced) {
		log.Error("Failed to sync informer cache")
		return err
	}
	return nil
}

func getClientset() (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	config, err = rest.InClusterConfig()
	if err != nil {
		log.Info("In-cluster config failed", zap.Error(err))
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfigFlag)
		if err != nil {
			log.Error("Failed load Kubernetes config", zap.Error(err))
			return nil, err
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Error("Error creating Kubernetes client", zap.Error(err))
		return nil, err
	}
	return clientset, nil
}

func deleteIngressMap(obj interface{}) {
	i, ok := obj.(*networkingv1.Ingress)
	if !ok {
		log.Debug("failed to cast to Ingress, skipping...")
		return
	}
	if len(i.Status.LoadBalancer.Ingress) == 0 {
		log.Debug("no LoadBalancer.Ingress, skipping...", zap.String("name", i.Name))
		return
	}
	for _, rule := range i.Spec.Rules {
		delete(ingressMap, rule.Host)
		log.Debug("Cleared up host",
			zap.String("host", rule.Host),
		)
	}
	log.Debug("Ingress deleted", zap.String("name", i.Name))
}

func updateIngressMap(obj interface{}) {
	i, ok := obj.(*networkingv1.Ingress)
	if !ok {
		log.Debug("failed to cast to Ingress, skipping...")
		return
	}
	if len(i.Status.LoadBalancer.Ingress) == 0 {
		log.Debug("no LoadBalancer.Ingress, skipping...", zap.String("name", i.Name))
		return
	}

	ip := i.Status.LoadBalancer.Ingress[0].IP
	for _, rule := range i.Spec.Rules {
		ingressMap[rule.Host] = ip
		log.Debug("Host discovered",
			zap.String("host", rule.Host),
			zap.String("ip", ip),
		)
	}
	log.Info("Ingress loaded",
		zap.String("name", i.Name),
		zap.String("namespace", i.Namespace),
		zap.String("load balancer", ip),
	)
}

func handleHttpRequest(w http.ResponseWriter, r *http.Request) {
	log.Debug("HTTP request", zap.String("uri", r.RequestURI))
	w.WriteHeader(http.StatusNoContent)
	w.Write([]byte{})
}

func handleDnsRequest(w dns.ResponseWriter, r *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	for _, q := range r.Question {
		log.Debug("Question",
			zap.String("name", q.Name),
			zap.String("type", dns.TypeToString[q.Qtype]),
		)

		if q.Qtype == dns.TypeA {
			host := strings.TrimSuffix(q.Name, ".")
			if ip, ok := ingressMap[host]; ok {
				a := &dns.A{
					Hdr: dns.RR_Header{
						Name:   q.Name,
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    300,
					},
					A: net.ParseIP(ip),
				}
				msg.Answer = append(msg.Answer, a)
				w.WriteMsg(msg)
				return
			}
		}

		if q.Qtype == dns.TypeAAAA {
			host := strings.TrimSuffix(q.Name, ".")
			if _, ok := ingressMap[host]; ok {
				msg.Answer = nil
				w.WriteMsg(msg)
				return
			}
		}
	}

	log.Debug("Not an Ingress host",
		zap.String("host", r.Question[0].Name),
	)
	forwardDnsRequest(w, r)
}

func forwardDnsRequest(w dns.ResponseWriter, r *dns.Msg) {
	for _, u := range upstreamsList {
		var resp *dns.Msg
		var err error
		var rrt time.Duration

		log.Debug("Calling upstream",
			zap.String("upstream", u),
			zap.String("question", r.Question[0].String()),
		)

		switch true {
		case strings.HasPrefix(u, "udp://") || strings.Index(u, "://") <= 0:
			h := strings.TrimPrefix(strings.TrimPrefix(u, "udp://"), "://")
			resp, rrt, err = dnsUdpClient.Exchange(r, h)
		case strings.HasPrefix(u, "tcp://"):
			h := strings.TrimPrefix(u, "tcp://")
			resp, rrt, err = dnsTcpClient.Exchange(r, h)
		default:
			log.Error("Unknown upstream format",
				zap.String("question", r.Question[0].String()),
				zap.String("upstream", u),
			)
			err = fmt.Errorf("unable to parse upstream: '%s'", u)
			resp = nil
		}

		q := strings.Replace(r.Question[0].String(), "\t", " ", -1)
		if err != nil {
			log.Warn("Upstream exchange failure",
				zap.String("upstream", u),
				zap.String("question", q),
				zap.Duration("rrt", rrt),
				zap.Error(err),
			)
			continue
		}
		if len(resp.Answer) > 0 {
			a := strings.Replace(resp.Answer[0].String(), "\t", " ", -1)
			log.Debug("Upstream response received",
				zap.String("upstream", u),
				zap.String("question", q),
				zap.String("answer", a),
			)
		} else {
			log.Debug("Upstream response is empty",
				zap.String("upstream", u),
				zap.String("question", q),
			)
		}
		w.WriteMsg(resp)
		return
	}

	log.Warn("All upstreams are dead, failing the request")
	m := &dns.Msg{}
	m.SetReply(r).SetRcode(r, dns.RcodeServerFailure)
	w.WriteMsg(m)
}
