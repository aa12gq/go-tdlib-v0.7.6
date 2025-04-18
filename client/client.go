package client

import (
	"context"
	"errors"
	"log"
	"runtime"
	"sync"
	"time"
)

type Client struct {
	jsonClient     *JsonClient
	extraGenerator ExtraGenerator
	responses      chan *Response
	listenerStore  *listenerStore
	catchersStore  *sync.Map
	updatesTimeout time.Duration
	catchTimeout   time.Duration
}

type Option func(*Client)

func WithExtraGenerator(extraGenerator ExtraGenerator) Option {
	return func(client *Client) {
		client.extraGenerator = extraGenerator
	}
}

func WithCatchTimeout(timeout time.Duration) Option {
	return func(client *Client) {
		client.catchTimeout = timeout
	}
}

func WithProxy(req *AddProxyRequest) Option {
	return func(client *Client) {
		client.AddProxy(req)
	}
}

func WithLogVerbosity(req *SetLogVerbosityLevelRequest) Option {
	return func(client *Client) {
		client.SetLogVerbosityLevel(req)
	}
}

func NewClient(authorizationStateHandler AuthorizationStateHandler, options ...Option) (*Client, error) {
	client := &Client{
		jsonClient:    NewJsonClient(),
		responses:     make(chan *Response, 1000),
		listenerStore: newListenerStore(),
		catchersStore: &sync.Map{},
	}

	client.extraGenerator = UuidV4Generator()
	client.catchTimeout = 60 * time.Second

	tdlibInstance.addClient(client)
	go client.receiver()

	for _, option := range options {
		go option(client)
	}

	err := Authorize(client, authorizationStateHandler)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func (client *Client) receiver() {
	defer func() {
		// Recover from any panics that might occur when sending to closed channels
		if r := recover(); r != nil {
			log.Printf("Recovered from panic in receiver: %v", r)
		}
	}()

	for response := range client.responses {
		// Handle responses with an Extra field (responses to specific requests)
		if response.Extra != "" {
			value, ok := client.catchersStore.Load(response.Extra)
			if ok {
				// Use a select with default to avoid blocking if the channel is full
				select {
				case value.(chan *Response) <- response:
					// Successfully sent
				default:
					// Channel is full or closed, skip
					log.Printf("Warning: Could not send response to catcher for extra: %s", response.Extra)
				}
			}
		}

		// Unmarshal the response data
		typ, err := UnmarshalType(response.Data)
		if err != nil {
			continue
		}

		// Check if this is a closing state update
		isClosingState := typ.GetType() == TypeUpdateAuthorizationState &&
			typ.(*UpdateAuthorizationState).AuthorizationState.AuthorizationStateType() == TypeAuthorizationStateClosed

		// Send the update to all active listeners
		needGc := false
		for _, listener := range client.listenerStore.Listeners() {
			if listener.IsActive() {
				// Use a select with default to avoid blocking if the channel is full
				select {
				case listener.Updates <- typ:
					// Successfully sent
				default:
					// Channel is full or closed, skip
					log.Printf("Warning: Could not send update to listener")
				}
			} else {
				needGc = true
			}
		}

		// Clean up inactive listeners if needed
		if needGc {
			client.listenerStore.gc()
		}

		// If we received a closed state, exit the receiver loop
		if isClosingState {
			log.Printf("Client %d received closed state, exiting receiver", client.jsonClient.id)
			return
		}
	}

	log.Printf("Receiver for client %d exited", client.jsonClient.id)
}

func (client *Client) Send(req Request) (*Response, error) {
	req.Extra = client.extraGenerator()

	catcher := make(chan *Response, 1)

	client.catchersStore.Store(req.Extra, catcher)

	defer func() {
		client.catchersStore.Delete(req.Extra)
		close(catcher)
	}()

	client.jsonClient.Send(req)

	ctx, cancel := context.WithTimeout(context.Background(), client.catchTimeout)
	defer cancel()

	select {
	case response := <-catcher:
		return response, nil

	case <-ctx.Done():
		return nil, errors.New("response catching timeout")
	}
}

func (client *Client) GetListener() *Listener {
	listener := &Listener{
		isActive: true,
		Updates:  make(chan Type, 1000),
	}
	client.listenerStore.Add(listener)

	return listener
}

// CloseAndCleanup closes the TDLib instance and cleans up all resources
func (client *Client) CloseAndCleanup() error {
	// First send the close request to TDLib
	_, err := client.Close()
	if err != nil {
		// Even if there's an error, continue with cleanup
		log.Printf("Error closing TDLib client: %v", err)
	}

	// Close all listeners first to stop any goroutines that might be using the client
	client.listenerStore.clear()

	// Clear all catchers to prevent memory leaks
	client.catchersStore.Range(func(key, value interface{}) bool {
		catcher := value.(chan *Response)
		close(catcher)
		client.catchersStore.Delete(key)
		return true
	})
	client.catchersStore = &sync.Map{}

	// Close the responses channel to stop the receiver goroutine
	close(client.responses)

	// Close the JsonClient to release C resources
	if client.jsonClient != nil {
		client.jsonClient.Close()
	}

	// Remove the client from the global instance
	tdlibInstance.removeClient(client)

	// Force garbage collection to help release memory
	runtime.GC()

	return nil
}
