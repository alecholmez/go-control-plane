package delta

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server is a wrapper interface which is meant to hold the proper stream handler for each xDS protocol.
type Server interface {
	DeltaStreamHandler(stream stream.DeltaStream, typeURL string) error
}

type Callbacks interface {
	// OnDeltaStreamOpen is called once an incremental xDS stream is open with a stream ID and the type URL (or "" for ADS).
	// Returning an error will end processing and close the stream. OnStreamClosed will still be called.
	OnDeltaStreamOpen(context.Context, int64, string) error
	// OnDeltaStreamClosed is called immediately prior to closing an xDS stream with a stream ID.
	OnDeltaStreamClosed(int64)
	// OnStreamDeltaRequest is called once a request is received on a stream.
	// Returning an error will end processing and close the stream. OnStreamClosed will still be called.
	OnStreamDeltaRequest(int64, *discovery.DeltaDiscoveryRequest) error
	// OnStreamDelatResponse is called immediately prior to sending a response on a stream.
	OnStreamDeltaResponse(int64, *discovery.DeltaDiscoveryRequest, *discovery.DeltaDiscoveryResponse)
}

type server struct {
	cache     cache.ConfigWatcher
	callbacks Callbacks

	// total stream count for counting bi-di streams
	streamCount int64
	ctx         context.Context
}

// watches for all delta xDS resource types
type watches struct {
	mu *sync.RWMutex

	deltaEndpoints chan cache.DeltaResponse
	deltaClusters  chan cache.DeltaResponse
	deltaRoutes    chan cache.DeltaResponse
	deltaListeners chan cache.DeltaResponse
	deltaSecrets   chan cache.DeltaResponse
	deltaRuntimes  chan cache.DeltaResponse

	deltaEndpointCancel func()
	deltaClusterCancel  func()
	deltaRouteCancel    func()
	deltaListenerCancel func()
	deltaSecretCancel   func()
	deltaRuntimeCancel  func()

	deltaEndpointNonce string
	deltaClusterNonce  string
	deltaRouteNonce    string
	deltaListenerNonce string
	deltaSecretNonce   string
	deltaRuntimeNonce  string

	// Organize stream state by resource type
	deltaStreamStates map[string]stream.StreamState

	// Opaque resources share a muxed channel. Nonces and watch cancellations are indexed by type URL.
	deltaResponses     chan cache.DeltaResponse
	deltaCancellations map[string]func()
	deltaNonces        map[string]string
	deltaTerminations  map[string]chan struct{}
}

// Initialize all watches
func (values *watches) Init() {
	// muxed channel needs a buffer to release go-routines populating it
	values.deltaResponses = make(chan cache.DeltaResponse, 6)
	values.deltaNonces = make(map[string]string)
	values.deltaTerminations = make(map[string]chan struct{})
	values.deltaCancellations = make(map[string]func())
	values.deltaStreamStates = make(map[string]stream.StreamState, int(types.UnknownType))
	values.mu = &sync.RWMutex{}
}

var deltaErrorResponse = &cache.RawDeltaResponse{}

// Cancel all watches
func (values *watches) Cancel() {
	if values.deltaEndpointCancel != nil {
		values.deltaEndpointCancel()
	}
	if values.deltaClusterCancel != nil {
		values.deltaClusterCancel()
	}
	if values.deltaRouteCancel != nil {
		values.deltaRouteCancel()
	}
	if values.deltaListenerCancel != nil {
		values.deltaListenerCancel()
	}
	if values.deltaSecretCancel != nil {
		values.deltaSecretCancel()
	}
	if values.deltaRuntimeCancel != nil {
		values.deltaRuntimeCancel()
	}
	for _, cancel := range values.deltaCancellations {
		if cancel != nil {
			cancel()
		}
	}
	for _, terminate := range values.deltaTerminations {
		close(terminate)
	}
}

// NewServer creates a delta xDS specific server which utilizes a ConfigWatcher and delta Callbacks.
func NewServer(ctx context.Context, config cache.ConfigWatcher, callbacks Callbacks) Server {
	return &server{
		cache:     config,
		callbacks: callbacks,
		ctx:       ctx,
	}
}

func (s *server) processDelta(str stream.DeltaStream, reqCh <-chan *discovery.DeltaDiscoveryRequest, defaultTypeURL string) error {
	// increment stream count
	streamID := atomic.AddInt64(&s.streamCount, 1)

	// unique nonce generator for req-resp pairs per xDS stream; the server
	// ignores stale nonces. nonce is only modified within send() function.
	var streamNonce int64

	// a collection of stack alloceated watches per request type
	var values watches
	values.Init()

	defer func() {
		values.Cancel()
		if s.callbacks != nil {
			s.callbacks.OnDeltaStreamClosed(streamID)
		}
	}()

	// sends a response by serializing to protobuf Any
	send := func(resp cache.DeltaResponse, typeURL string) (string, error) {
		if resp == nil {
			return "", errors.New("missing response")
		}

		out, err := resp.GetDeltaDiscoveryResponse()
		if err != nil {
			return "", err
		}

		// increment nonce
		streamNonce = streamNonce + 1
		out.Nonce = strconv.FormatInt(streamNonce, 10)
		if s.callbacks != nil {
			s.callbacks.OnStreamDeltaResponse(streamID, resp.GetDeltaRequest(), out)
		}

		return out.Nonce, str.Send(out)
	}

	// setState updates the currently known state of resources in the server
	setState := func(res cache.DeltaResponse, typeURL string) {
		values.mu.Lock()
		defer values.mu.Unlock()

		state := values.deltaStreamStates[typeURL]
		if state.ResourceVersions == nil {
			state.ResourceVersions = make(map[string]string)
		}
		state.ResourceVersions = res.GetNextVersionMap()
		values.deltaStreamStates[typeURL] = state
	}

	if s.callbacks != nil {
		if err := s.callbacks.OnDeltaStreamOpen(str.Context(), streamID, defaultTypeURL); err != nil {
			return err
		}
	}

	// node may only be set on the first discovery request
	var node = &core.Node{}
	isWildcard := map[string]bool{}

	for {
		select {
		case <-s.ctx.Done():
			return nil
			// config watcher can send the requested resources types in any order
		case resp, more := <-values.deltaEndpoints:
			if !more {
				return status.Errorf(codes.Unavailable, "endpoints watch failed")
			}
			nonce, err := send(resp, resource.EndpointType)
			if err != nil {
				return err
			}
			values.deltaEndpointNonce = nonce
			setState(resp, resource.EndpointType)
		case resp, more := <-values.deltaClusters:
			if !more {
				return status.Errorf(codes.Unavailable, "clusters watch failed")
			}
			nonce, err := send(resp, resource.ClusterType)
			if err != nil {
				return err
			}
			values.deltaClusterNonce = nonce
			setState(resp, resource.ClusterType)
		case resp, more := <-values.deltaRoutes:
			if !more {
				return status.Errorf(codes.Unavailable, "routes watch failed")
			}
			nonce, err := send(resp, resource.RouteType)
			if err != nil {
				return err
			}
			values.deltaRouteNonce = nonce
			setState(resp, resource.RouteType)
		case resp, more := <-values.deltaListeners:
			if !more {
				return status.Errorf(codes.Unavailable, "listeners watch failed")
			}
			nonce, err := send(resp, resource.ListenerType)
			if err != nil {
				return err
			}
			values.deltaListenerNonce = nonce
			setState(resp, resource.ListenerType)
		case resp, more := <-values.deltaSecrets:
			if !more {
				return status.Errorf(codes.Unavailable, "secrets watch failed")
			}
			nonce, err := send(resp, resource.SecretType)
			if err != nil {
				return err
			}
			values.deltaSecretNonce = nonce
			setState(resp, resource.SecretType)
		case resp, more := <-values.deltaRuntimes:
			if !more {
				return status.Errorf(codes.Unavailable, "runtimes watch failed")
			}
			nonce, err := send(resp, resource.RuntimeType)
			if err != nil {
				return err
			}
			values.deltaRuntimeNonce = nonce
			setState(resp, resource.RuntimeType)
		case resp, more := <-values.deltaResponses:
			if more {
				if resp == deltaErrorResponse {
					return status.Errorf(codes.Unavailable, "delta resource watch failed")
				}
				typeURL := resp.GetDeltaRequest().TypeUrl
				nonce, err := send(resp, typeURL)
				if err != nil {
					return err
				}
				values.deltaNonces[typeURL] = nonce
				setState(resp, typeURL)
			}
		case req, more := <-reqCh:
			// input stream ended or errored out
			if !more {
				return nil
			}
			if req == nil {
				return status.Errorf(codes.Unavailable, "empty request")
			}

			// Log out our error detail from  envoyif we get one but don't do anything crazy here yet
			// TODO: embed a logger in the server so we can be a bit more verbose when needed
			if req.ErrorDetail != nil {
				return status.Errorf(codes.Code(req.ErrorDetail.GetCode()), "received error from xDS client: %s", req.ErrorDetail.GetMessage())
			}

			// node field in discovery request is delta-compressed
			// nonces can be reused across streams; we verify nonce only if nonce is not initialized
			var nonce string
			if req.Node != nil {
				node = req.Node
				nonce = req.GetResponseNonce()
			} else {
				req.Node = node
				// If we have no nonce, i.e. this is the first request on a delta stream, set one
				nonce = strconv.FormatInt(streamNonce, 10)
			}

			// type URL is required for ADS but is implicit for xDS
			if defaultTypeURL == resource.AnyType {
				if req.TypeUrl == "" {
					return status.Errorf(codes.InvalidArgument, "type URL is required for ADS")
				}
			} else if req.TypeUrl == "" {
				req.TypeUrl = defaultTypeURL
			}

			state := values.deltaStreamStates[req.GetTypeUrl()]
			// If this is empty we can assume this is the first time state
			// is being set on this stream for this resource type
			if state.ResourceVersions == nil {
				state.ResourceVersions = make(map[string]string)
			}

			// We are in the wildcard mode if the first request of a particular type has an empty subscription list
			var found bool
			if state.IsWildcard, found = isWildcard[req.TypeUrl]; !found {
				state.IsWildcard = len(req.GetResourceNamesSubscribe()) == 0
				isWildcard[req.TypeUrl] = state.IsWildcard
			}

			if sub := req.GetResourceNamesSubscribe(); len(sub) > 0 {
				s.subscribe(sub, state.ResourceVersions)
			}
			for r, v := range req.InitialResourceVersions {
				state.ResourceVersions[r] = v
			}
			if unsub := req.GetResourceNamesUnsubscribe(); len(unsub) > 0 {
				s.unsubscribe(unsub, state.ResourceVersions)
			}

			if s.callbacks != nil {
				if err := s.callbacks.OnStreamDeltaRequest(streamID, req); err != nil {
					return err
				}
			}

			// cancel existing watches to (re-)request a newer version
			switch {
			case req.TypeUrl == resource.EndpointType:
				if values.deltaEndpointNonce == "" || values.deltaEndpointNonce == nonce {
					if values.deltaEndpointCancel != nil {
						values.deltaEndpointCancel()
					}
					values.deltaEndpoints, values.deltaEndpointCancel = s.cache.CreateDeltaWatch(req, &state)
				}
			case req.TypeUrl == resource.ClusterType:
				if values.deltaClusterNonce == "" || values.deltaClusterNonce == nonce {
					if values.deltaClusterCancel != nil {
						values.deltaClusterCancel()
					}
					values.deltaClusters, values.deltaClusterCancel = s.cache.CreateDeltaWatch(req, &state)
				}
			case req.TypeUrl == resource.RouteType:
				if values.deltaRouteNonce == "" || values.deltaRouteNonce == nonce {
					if values.deltaRouteCancel != nil {
						values.deltaRouteCancel()
					}
					values.deltaRoutes, values.deltaRouteCancel = s.cache.CreateDeltaWatch(req, &state)
				}
			case req.TypeUrl == resource.ListenerType:
				if values.deltaListenerNonce == "" || values.deltaListenerNonce == nonce {
					if values.deltaListenerCancel != nil {
						values.deltaListenerCancel()
					}
					values.deltaListeners, values.deltaListenerCancel = s.cache.CreateDeltaWatch(req, &state)
				}
			case req.TypeUrl == resource.SecretType:
				if values.deltaSecretNonce == "" || values.deltaSecretNonce == nonce {
					if values.deltaSecretCancel != nil {
						values.deltaSecretCancel()
					}
					values.deltaSecrets, values.deltaSecretCancel = s.cache.CreateDeltaWatch(req, &state)
				}
			case req.TypeUrl == resource.RuntimeType:
				if values.deltaRuntimeNonce == "" || values.deltaRuntimeNonce == nonce {
					if values.deltaRuntimeCancel != nil {
						values.deltaRuntimeCancel()
					}
					values.deltaRuntimes, values.deltaRuntimeCancel = s.cache.CreateDeltaWatch(req, &state)
				}
			default:
				typeURL := req.TypeUrl
				responseNonce, seen := values.deltaNonces[typeURL]
				if !seen || responseNonce == nonce {
					// We must signal goroutine termination to prevent a race between the cancel closing the watch
					// and the producer closing the watch.
					if terminate, exists := values.deltaTerminations[typeURL]; exists {
						close(terminate)
					}
					if cancel, seen := values.deltaCancellations[typeURL]; seen && cancel != nil {
						cancel()
					}

					var watch chan cache.DeltaResponse
					watch, values.deltaCancellations[typeURL] = s.cache.CreateDeltaWatch(req, &state)

					// a go-routine. Golang does not allow selecting over a dynamic set of channels.
					terminate := make(chan struct{})
					values.deltaTerminations[typeURL] = terminate
					go func() {
						select {
						case resp, more := <-watch:
							if more {
								values.deltaResponses <- resp
							} else {
								// Check again if the watch is cancelled.
								select {
								case <-terminate: // do nothing
								default:
									// We cannot close the responses channel since it can be closed twice.
									// Instead we send a fake error response.
									values.deltaResponses <- deltaErrorResponse
								}
							}
							break
						case <-terminate:
							break
						}
					}()
				}
			}
		}
	}
}

func (s *server) DeltaStreamHandler(str stream.DeltaStream, typeURL string) error {
	// a channel for receiving incoming delta requests
	reqCh := make(chan *discovery.DeltaDiscoveryRequest)
	reqStop := int32(0)

	go func() {
		for {
			req, err := str.Recv()
			if atomic.LoadInt32(&reqStop) != 0 {
				return
			}
			if err != nil {
				close(reqCh)
				return
			}

			reqCh <- req
		}
	}()

	err := s.processDelta(str, reqCh, typeURL)
	atomic.StoreInt32(&reqStop, 1)

	return err
}

// When we subscribe, we just want to make the cache know we are subscribing to a resource.
// Providing a name with an empty version is enough to make that happen.
func (s *server) subscribe(resources []string, sv map[string]string) {
	for _, resource := range resources {
		sv[resource] = ""
	}
}

// When we unsubscribe, we need to search and remove from the current subscribed list in the servers state
// so when we send that down to the cache, it knows to no longer track that resource
func (s *server) unsubscribe(resources []string, sv map[string]string) {
	for _, resource := range resources {
		delete(sv, resource)
	}
}
