// Command pubsubfake runs the in-process fake Pub/Sub server of the Pub/Sub library as a program, on a local port, for the smoke
// tests of the real binaries when no emulator is installed. It prints its address (host:port) on the first line of its output, and
// runs until it is stopped. Point PUBSUB_EMULATOR_HOST at the address.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"cloud.google.com/go/pubsub/v2/pstest"
)

func main() {
	srv := pstest.NewServer()
	defer srv.Close()
	fmt.Println(srv.Addr)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
}
