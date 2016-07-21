package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/opsee/basic/schema"
	"github.com/opsee/basic/service"
	"github.com/opsee/hailcannon/hacker"
	"github.com/opsee/hailcannon/svc"
	log "github.com/opsee/logrus"
	"github.com/opsee/spanx/spanxcreds"
	"google.golang.org/grpc/credentials"
)

const (
	GrpcTimeout = 15 * time.Second
)

var (
	signalsChannel = make(chan os.Signal, 1)
)

func init() {
	signal.Notify(signalsChannel, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
}

type ActiveHackers struct {
	Hackers   map[string]*hacker.Hacker
	staleKeys chan string
	sync.Mutex
}

func NewActiveHackers() *ActiveHackers {
	ah := &ActiveHackers{
		Hackers:   make(map[string]*hacker.Hacker),
		staleKeys: make(chan string),
	}
	go ah.removeStaleKeys()
	return ah
}

func (ah *ActiveHackers) Get(key string) *hacker.Hacker {
	ah.Lock()
	defer ah.Unlock()
	if h, ok := ah.Hackers[key]; ok {
		return h
	}
	return nil
}

func (ah *ActiveHackers) Delete(key string) {
	ah.Lock()
	defer ah.Unlock()
	delete(ah.Hackers, key)
}

func (ah *ActiveHackers) Put(key string, h *hacker.Hacker) {
	ah.Lock()
	defer ah.Unlock()
	ah.Hackers[key] = h
}

// channel to report stale keys to be removed from the map
// hackers can exit asynchronously due to cloudformation update errors
func (ah *ActiveHackers) StaleKeys() chan string {
	return ah.staleKeys
}

func (ah *ActiveHackers) removeStaleKeys() {
	for {
		select {
		case customerId := <-ah.staleKeys:
			ah.Delete(customerId)
			log.Debugf("Hacker for customer %s stopped.  Deleting from index.", customerId)
		}
	}
}

// TODO(dan) grpc endpoint when we need it
// dummy endpoint to prevent ECS kills
func hello(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "hello")
}

func health() {
	hailcannonAddress := os.Getenv("HAILCANNON_ADDRESS")
	log.Infof("Listening on %s", hailcannonAddress)
	http.HandleFunc("/health", hello)
	panic(http.ListenAndServe(hailcannonAddress, nil))
}

func logLevel(defaultLevel log.Level) {
	level, err := log.ParseLevel(os.Getenv("HAILCANNON_LOG_LEVEL"))
	if err != nil {
		log.WithError(err).Error("Couldn't set log level")
		level = defaultLevel
	}
	log.SetLevel(level)
}

func main() {
	ah := NewActiveHackers()
	services := svc.NewOpseeServices()

	go health()

	spanxConn, err := grpc.Dial(os.Getenv("HAILCANNON_SPANX_ADDRESS"),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
		grpc.WithTimeout(GrpcTimeout))
	if err != nil {
		log.WithError(err).Fatal("Couldn't create grpc connection to spanx.")
	}
	spanxClient := service.NewSpanxClient(spanxConn)
	defer spanxConn.Close()

	bezosConn, err := grpc.Dial(os.Getenv("HAILCANNON_BEZOSPHERE_ADDRESS"),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
		grpc.WithTimeout(GrpcTimeout))
	if err != nil {
		log.WithError(err).Fatal("Couldn't create grpc connection to bezosphere.")
	}

	if err != nil {
		log.Fatalf("Couldn't initialize connection to bezosphere")
	}

	bezosClient := service.NewBezosClient(bezosConn)
	defer bezosConn.Close()

	for {
		select {
		case s := <-signalsChannel:
			switch s {
			case syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT:
				log.Infof("Received signal %s, Stopping.", s)
				os.Exit(0)
			}
		case <-time.After(30 * time.Second):
			activeBastions, err := services.GetBastionStates([]string{}, &service.Filter{Key: "status", Value: "active"})
			if err != nil {
				log.WithError(err).Error("couldn't get bastion states.")
				continue
			}
			for _, bastion := range activeBastions {
				if ah.Get(bastion.CustomerId) == nil {
					config := &hacker.Config{
						VpcId:      bastion.VpcId,
						CustomerId: bastion.CustomerId,
						Region:     bastion.Region,
					}
					resources := &hacker.Resources{
						BastionStackPhysicalId: fmt.Sprintf("opsee-stack-%s", bastion.CustomerId),
					}

					creds := spanxcreds.NewSpanxCredentials(&schema.User{CustomerId: bastion.CustomerId}, spanxClient)
					sess := session.New(&aws.Config{
						Credentials: creds,
						Region:      aws.String(region),
					})

					clients := &hacker.Clients{
						Ec2:            ec2.New(sess),
						Cloudformation: cloudformation.New(sess),
						Bezos:          beosClient,
					}

					nh, err := hacker.New(config, creds, clients, ah.StaleKeys())
					if err != nil {
						log.WithError(err).Errorf("couldn't create new hacker for customer %s", bastion.CustomerId)
						continue
					}

					log.Infof("created hacker for customer %s", bastion.CustomerId)
					ah.Put(bastion.CustomerId, nh)
					go nh.Start()
				}
			}
		}
	}
}
