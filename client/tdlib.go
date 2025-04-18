package client

/*
#include <stdlib.h>
#include <td/telegram/td_json_client.h>
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

var tdlibInstance *tdlib

func init() {
	tdlibInstance = &tdlib{
		timeout: 60 * time.Second,
		clients: map[int]*Client{},
	}
}

type tdlib struct {
	once    sync.Once
	timeout time.Duration
	mu      sync.Mutex
	clients map[int]*Client
}

func (instance *tdlib) addClient(client *Client) {
	instance.mu.Lock()
	defer instance.mu.Unlock()

	instance.clients[client.jsonClient.id] = client

	instance.once.Do(func() {
		go instance.receiver()
	})
}

func (instance *tdlib) getClient(id int) (*Client, error) {
	instance.mu.Lock()
	defer instance.mu.Unlock()

	client, ok := instance.clients[id]
	if !ok {
		return nil, fmt.Errorf("client [id: %d] does not exist", id)
	}

	return client, nil
}

func (instance *tdlib) receiver() {
	for {
		resp, err := instance.receive(instance.timeout)
		if err != nil {
			continue
		}

		client, err := instance.getClient(resp.ClientId)
		if err != nil {
			log.Print(err)
			continue
		}

		client.responses <- resp
	}
}

// Receives incoming updates and request responses from the TDLib client. May be called from any thread, but
// shouldn't be called simultaneously from two different threads.
// Returned pointer will be deallocated by TDLib during next call to td_json_client_receive or td_json_client_execute
// in the same thread, so it can't be used after that.
func (instance *tdlib) receive(timeout time.Duration) (*Response, error) {
	result := C.td_receive(C.double(float64(timeout) / float64(time.Second)))
	if result == nil {
		return nil, errors.New("update receiving timeout")
	}

	data := []byte(C.GoString(result))

	var resp Response

	err := json.Unmarshal(data, &resp)
	if err != nil {
		return nil, err
	}

	resp.Data = data

	return &resp, nil
}

func Execute(req Request) (*Response, error) {
	data, _ := json.Marshal(req)

	query := C.CString(string(data))
	defer C.free(unsafe.Pointer(query))
	result := C.td_execute(query)
	if result == nil {
		return nil, errors.New("request can't be parsed")
	}

	data = []byte(C.GoString(result))

	var resp Response

	err := json.Unmarshal(data, &resp)
	if err != nil {
		return nil, err
	}

	resp.Data = data

	return &resp, nil
}

type JsonClient struct {
	id int
}

func NewJsonClient() *JsonClient {
	return &JsonClient{
		id: int(C.td_create_client_id()),
	}
}

// Sends request to the TDLib client. May be called from any thread.
func (jsonClient *JsonClient) Send(req Request) {
	data, _ := json.Marshal(req)

	query := C.CString(string(data))
	defer C.free(unsafe.Pointer(query))

	C.td_send(C.int(jsonClient.id), query)
}

// Synchronously executes TDLib request. May be called from any thread.
// Only a few requests can be executed synchronously.
// Returned pointer will be deallocated by TDLib during next call to td_json_client_receive or td_json_client_execute
// in the same thread, so it can't be used after that.
func (jsonClient *JsonClient) Execute(req Request) (*Response, error) {
	return Execute(req)
}

// Close marks this client as closed and releases all associated resources
// TDLib instances are destroyed automatically after they are closed
func (jsonClient *JsonClient) Close() {
	// Only close if the client ID is valid
	if jsonClient.id >= 0 {
		// Send a close request to TDLib
		query := C.CString(`{"@type":"close"}`)
		defer C.free(unsafe.Pointer(query))
		C.td_send(C.int(jsonClient.id), query)

		// Force garbage collection to release any C memory that might be held by Go
		runtime.GC()

		// Mark the client as invalid
		jsonClient.id = -1
	}
}

type meta struct {
	Type     string `json:"@type"`
	Extra    string `json:"@extra"`
	ClientId int    `json:"@client_id"`
}

type Request struct {
	meta
	Data map[string]interface{}
}

func (req Request) MarshalJSON() ([]byte, error) {
	req.Data["@type"] = req.Type
	req.Data["@extra"] = req.Extra

	return json.Marshal(req.Data)
}

type Response struct {
	meta
	Data json.RawMessage
}

type ResponseError struct {
	Err *Error
}

func (responseError ResponseError) Error() string {
	return fmt.Sprintf("%d %s", responseError.Err.Code, responseError.Err.Message)
}

func buildResponseError(data json.RawMessage) error {
	respErr, err := UnmarshalError(data)
	if err != nil {
		return err
	}

	return ResponseError{
		Err: respErr,
	}
}

// JsonInt64 alias for int64, in order to deal with json big number problem
type JsonInt64 int64

// MarshalJSON marshals to json
func (jsonInt64 JsonInt64) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(jsonInt64), 10) + `"`), nil
}

// UnmarshalJSON unmarshals from json
func (jsonInt64 *JsonInt64) UnmarshalJSON(data []byte) error {
	if len(data) > 2 && data[0] == '"' && data[len(data)-1] == '"' {
		data = data[1 : len(data)-1]
	}

	jsonBigInt, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return err
	}

	*jsonInt64 = JsonInt64(jsonBigInt)

	return nil
}

type Type interface {
	GetType() string
	GetClass() string
}

// removeClient removes a client from the tdlib instance and performs cleanup
func (instance *tdlib) removeClient(client *Client) {
	instance.mu.Lock()
	defer instance.mu.Unlock()

	// Remove the client from the map
	if client != nil && client.jsonClient != nil && client.jsonClient.id >= 0 {
		delete(instance.clients, client.jsonClient.id)
		log.Printf("Removed client with ID %d from tdlib instance", client.jsonClient.id)
	}

	// Check if there are no more clients and potentially clean up resources
	if len(instance.clients) == 0 {
		log.Printf("No more clients in tdlib instance")
		// Force garbage collection to help release memory
		runtime.GC()
	}
}
