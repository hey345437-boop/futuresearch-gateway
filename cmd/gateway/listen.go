package main

import "net"

// listen 单独拆出来，方便测试注入。
func listen(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
