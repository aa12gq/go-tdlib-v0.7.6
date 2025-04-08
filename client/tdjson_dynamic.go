//go:build libtdjson && (linux || darwin)
// +build libtdjson
// +build linux darwin

package client

/*
#cgo linux CFLAGS: -I/usr/local/include
#cgo linux LDFLAGS: -L/usr/local/lib -ltdjson -lstdc++ -lssl -lcrypto -ldl -lz -lm
#cgo darwin CFLAGS: -I/usr/local/include
#cgo darwin LDFLAGS: -L/usr/local/lib -ltdjson -lstdc++ -lssl -lcrypto -ldl -lz -lm
*/
import "C"
