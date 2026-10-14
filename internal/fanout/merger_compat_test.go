package fanout_test

import (
	"github.com/querdian-dev/querdian/internal/fanout"
	"github.com/querdian-dev/querdian/internal/proxy"
)

// The merge functions must stay assignable to proxy.Merger (internal/fanout itself cannot import proxy).
var (
	_ proxy.Merger = fanout.MergeLokiStreams
)
