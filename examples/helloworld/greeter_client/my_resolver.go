package main

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/resolver"
)

const scheme = "my"

type myBuilder struct{}

func (*myBuilder) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	if target.Endpoint() == "" && opts.Dialer == nil {
		return nil, errors.New("my resolver: received empty target in Build()")
	}
	r := &myResolver{
		target: target,
		cc:     cc,
	}
	r.start()
	return r, nil
}

func (*myBuilder) Scheme() string {
	return scheme
}

type myResolver struct {
	target resolver.Target
	cc     resolver.ClientConn
}

func (r *myResolver) start() {
	url := r.target.Endpoint()
	lists := strings.Split(url, ",")

	var addr []resolver.Address
	for _, list := range lists {
		addr = append(addr, resolver.Address{Addr: list})
	}
	fmt.Printf("addr: %v\n", addr)

	r.cc.UpdateState(resolver.State{Addresses: addr})
}

func (r *myResolver) ResolveNow(o resolver.ResolveNowOptions) {
	/*
		url := r.target.Endpoint()
		lists := strings.Split(url, ",")

		var addr []resolver.Address
		for _, list := range lists {
			addr = append(addr, resolver.Address{Addr: list})
		}
		fmt.Printf("addr: %v\n", addr)

		r.cc.UpdateState(resolver.State{Addresses: addr})
	*/
}

func (*myResolver) Close() {}

func init() {
	resolver.Register(&myBuilder{})
}
