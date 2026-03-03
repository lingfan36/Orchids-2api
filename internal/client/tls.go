package client

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"
)

func tlsDial(addr, serverName string) (net.Conn, error) {
	tcpConn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}

	tlsConn := tls.Client(tcpConn, &tls.Config{
		ServerName: serverName,
	})

	if err := tlsConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		tcpConn.Close()
		return nil, err
	}
	if err := tlsConn.Handshake(); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		tcpConn.Close()
		return nil, err
	}
	return tlsConn, nil
}
