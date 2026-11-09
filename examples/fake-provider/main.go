// Command fake-provider pretends to be OpenAI and Anthropic so you can try veilox without an API key.
// It prints every prompt it receives, so you can see what a provider would see.
//
//	go run ./examples/fake-provider --listen 127.0.0.1:9999
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jsxd3v/veilox/internal/fakeprovider"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9999", "address to listen on")
	flag.Parse()
	fake := &fakeprovider.Fake{OnRequest: func(path, prompt string) {
		fmt.Printf("── provider received (%s) ──\n%s\n\n", path, prompt)
	}}
	srv := &http.Server{Addr: *listen, Handler: fake, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fake provider on http://%s", *listen)
	log.Fatal(srv.ListenAndServe())
}
