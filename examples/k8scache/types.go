// Command k8scache keeps a cache of Pod objects, as a Kubernetes controller
// would, in five forms: ordinary Go pointers, a map of Compact values (alone
// and with a shared Dict) and a SlabMap (alone and with a shared Dict), and
// compares their footprint and the cost of a full collection.
package main

import (
	"net/netip"
	"time"
)

//go:generate go run github.com/Voskan/cnfgo/cmd/cnfgo -compact=Pod -codec=github.com/Voskan/cnfgo/examples/k8scache.Quantity=github.com/Voskan/cnfgo/cnfrt/codecs/k8s.Quantity[T,*T]

// Pod is a reduced form of the Kubernetes Pod object.
type Pod struct {
	Namespace   string
	Name        string
	UID         string
	Labels      map[string]string
	Annotations map[string]string
	NodeName    string
	Phase       PodPhase
	PodIP       netip.Addr
	StartTime   time.Time
	Containers  []Container
	Conditions  []Condition
	Priority    *int32
}

// PodPhase is the lifecycle phase of a pod.
type PodPhase uint8

// Phases.
const (
	Pending PodPhase = iota
	Running
	Succeeded
	Failed
)

// Container describes one container of a pod.
type Container struct {
	Name      string
	Image     string
	Args      []string
	Ports     []ContainerPort
	Resources Resources
}

// ContainerPort is a port exposed by a container.
type ContainerPort struct {
	Name     string
	Port     uint16
	Protocol string
}

// Resources lists the compute resources of a container.
type Resources struct {
	CPURequest    Quantity
	CPULimit      Quantity
	MemoryRequest Quantity
	MemoryLimit   Quantity
}

// Condition is an observed condition of a pod.
type Condition struct {
	Type               string
	Status             bool
	LastTransitionTime time.Time
}
