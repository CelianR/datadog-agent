// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build !windows

// UDS won't work in windows

package listeners

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/DataDog/datadog-agent/comp/core/config"
	"github.com/DataDog/datadog-agent/comp/dogstatsd/packets"
)

func udsStreamListenerFactory(packetOut chan packets.Packets, manager *packets.PoolManager, cfg config.Component) (StatsdListener, error) {
	return NewUDSStreamListener(packetOut, manager, nil, cfg, nil)
}

func TestNewUDSStreamListener(t *testing.T) {
	testNewUDSListener(t, udsStreamListenerFactory, "unix")
}

func TestStartStopUDSStreamListener(t *testing.T) {
	testStartStopUDSListener(t, udsStreamListenerFactory, "unix")
}

func TestUDSStreamReceive(t *testing.T) {
	socketPath := testSocketPath(t)

	mockConfig := map[string]interface{}{}
	mockConfig[socketPathConfKey("unix")] = socketPath
	mockConfig["dogstatsd_origin_detection"] = false

	var contents0 = []byte("daemon:666|g|#sometag1:somevalue1,sometag2:somevalue2")
	var contents1 = []byte("daemon:999|g|#sometag1:somevalue1")

	packetsChannel := make(chan packets.Packets)

	config := fulfillDepsWithConfig(t, mockConfig)
	s, err := udsStreamListenerFactory(packetsChannel, newPacketPoolManagerUDS(config), config)
	assert.Nil(t, err)
	assert.NotNil(t, s)

	s.Listen()
	defer s.Stop()
	conn, err := net.Dial("unix", socketPath)
	assert.Nil(t, err)
	defer conn.Close()

	binary.Write(conn, binary.LittleEndian, int32(len(contents0)))
	conn.Write(contents0)

	binary.Write(conn, binary.LittleEndian, int32(len(contents1)))
	conn.Write(contents1)

	select {
	case pkts := <-packetsChannel:
		assert.Equal(t, 2, len(pkts))

		packet := pkts[0]
		assert.NotNil(t, packet)
		assert.Equal(t, packet.Contents, contents0)
		assert.Equal(t, packet.Origin, "")
		assert.Equal(t, packet.Source, packets.UDS)

		packet = pkts[1]
		assert.NotNil(t, packet)
		assert.Equal(t, packet.Contents, contents1)
		assert.Equal(t, packet.Origin, "")
		assert.Equal(t, packet.Source, packets.UDS)

	case <-time.After(2 * time.Second):
		assert.FailNow(t, "Timeout on receive channel")
	}

}
