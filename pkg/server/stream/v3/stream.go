package stream

import (
	"google.golang.org/grpc"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
)

// Generic RPC stream.
type Stream interface {
	grpc.ServerStream

	Send(*discovery.DiscoveryResponse) error
	Recv() (*discovery.DiscoveryRequest, error)
}

type DeltaStream interface {
	grpc.ServerStream

	Send(*discovery.DeltaDiscoveryResponse) error
	Recv() (*discovery.DeltaDiscoveryRequest, error)
}

// StreamState will keep track of resource state on a stream
type StreamState struct {
	// ResourceVersions contains a hash of the resource as the value and the resource name as the key.
	// This field stores the last state sent to the client.
	ResourceVersions map[string]string
}

// NewStreamState initializes a stream state.
func NewStreamState() StreamState {
	return StreamState{
		ResourceVersions: make(map[string]string),
	}
}

// GetResourceVersions returns a new map snapshotted from the current state of the s.ResourceVersions map.
// We do this because maps are reference types in Go, and if we pass around the ResourceVersions map into the cache
// the state can be mutated across multiple threads creating a datarace.
func (s StreamState) GetResourceVersions() map[string]string {
	versions := make(map[string]string, len(s.ResourceVersions))
	for key, val := range s.ResourceVersions {
		versions[key] = val
	}

	return versions
}

func (s StreamState) SetResourceVersions(versions map[string]string) {
	s.ResourceVersions = versions
}
