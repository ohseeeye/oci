// Copyright 2023 CUE Labs AG
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ociserver"
)

func main() {
	if err := main1(); err != nil {
		fmt.Fprintf(os.Stderr, "oci: %v\n", err)
		os.Exit(1)
	}
}

var writeNetAddr func(l net.Listener)

func main1() error {
	listenAddr := flag.String("listen", "localhost:5000", "address on which to listen")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ocisrv [-listen address]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
	}

	r := ocimem.New()
	l, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		return fmt.Errorf("cannot listen on %q: %v", *listenAddr, err)
	}
	if writeNetAddr != nil {
		writeNetAddr(l)
	}
	fmt.Printf("listening on %v\n", l.Addr())
	srv, err := ociserver.New(r, nil)
	if err != nil {
		return fmt.Errorf("cannot construct server: %v", err)
	}
	err = http.Serve(l, srv)
	return fmt.Errorf("http server error: %v", err)
}
