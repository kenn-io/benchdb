package columnar

import (
	"context"
	"net"
	"sync/atomic"
)

// ArchiveTraffic is a remote archive's network use since the engine opened.
// A local archive reports zeros.
type ArchiveTraffic struct {
	// Connections, BytesSent, and BytesReceived count DuckDB's connections
	// to the provider through the archive proxy, including TLS overhead.
	// DuckDB reuses connections, so Connections counts new ones, not requests.
	Connections, BytesSent, BytesReceived int64
	// CatalogDownloads and CatalogBytes count the summary and lookup
	// catalogs the catalog cache fetched; cache hits fetch nothing.
	CatalogDownloads, CatalogBytes int64
}

// Since returns the traffic between an earlier reading and t.
func (t ArchiveTraffic) Since(earlier ArchiveTraffic) ArchiveTraffic {
	return ArchiveTraffic{
		Connections:      t.Connections - earlier.Connections,
		BytesSent:        t.BytesSent - earlier.BytesSent,
		BytesReceived:    t.BytesReceived - earlier.BytesReceived,
		CatalogDownloads: t.CatalogDownloads - earlier.CatalogDownloads,
		CatalogBytes:     t.CatalogBytes - earlier.CatalogBytes,
	}
}

// ArchiveTraffic reports the remote archive's network use so far.
func (e *Engine) ArchiveTraffic() ArchiveTraffic {
	return e.traffic.read()
}

type trafficCounters struct {
	connections, sent, received, catalogs, catalogBytes atomic.Int64
}

func (c *trafficCounters) read() ArchiveTraffic {
	return ArchiveTraffic{
		Connections:      c.connections.Load(),
		BytesSent:        c.sent.Load(),
		BytesReceived:    c.received.Load(),
		CatalogDownloads: c.catalogs.Load(),
		CatalogBytes:     c.catalogBytes.Load(),
	}
}

func (c *trafficCounters) catalogDownloaded(size int64) {
	c.catalogs.Add(1)
	c.catalogBytes.Add(size)
}

// dialer wraps the proxy's upstream dialer so every connection it opens is
// counted.
func (c *trafficCounters) dialer(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		c.connections.Add(1)
		return &countedConn{Conn: conn, c: c}, nil
	}
}

type countedConn struct {
	net.Conn
	c *trafficCounters
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.c.received.Add(int64(n))
	return n, err
}

func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.c.sent.Add(int64(n))
	return n, err
}
