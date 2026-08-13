package agent

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"dedup/internal/config"
	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

func TestPhase2RealTCPLoopbackSeparatesStageTwoAndStageThreePayloads(t *testing.T) {
	for _, test := range []struct {
		name    string
		stage   uint8
		field   uint32
		payload []byte
	}{
		{name: "stage-two", stage: proto.ScreenStageTwo, field: proto.FieldPHashParts, payload: []byte{2, 2}},
		{name: "stage-three", stage: proto.ScreenStageThree, field: proto.FieldSobelHist, payload: []byte{3, 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			pool := newPhase2FakePool()
			defer close(pool.results)
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			router := NewPoolRouter(pool, log)
			path := `D:\loopback\` + test.name + `.jpg`
			phase2 := NewPhase2ManagerWithRuntime(
				"machine-a",
				&phase2CommittedFake{states: map[string]store.Phase2Committed{path: {MissingFields: 0}}},
				pool, router,
				func(string) (int64, bool, error) { return 1, false, nil }, log,
			)
			defer phase2.Shutdown(context.Background())
			pool.onSubmit = func(job worker.JobMsg) {
				result := &worker.JobResultMsg{
					JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind,
					Phase: job.Phase, ScreenStage: job.ScreenStage, Source: job.Source,
					SHA512: append([]byte(nil), job.KnownSHA...), FieldsDone: job.FieldsMask,
				}
				if test.field == proto.FieldPHashParts {
					result.PHashParts = append([]byte(nil), test.payload...)
				} else {
					result.SobelHist = append([]byte(nil), test.payload...)
				}
				pool.results <- result
			}
			cfg := config.DefaultAgent()
			cfg.MachineID, cfg.Proto.HeartbeatS = "machine-a", 60
			server := NewServer(cfg, scanHandlerFunc(func(task proto.ScanTask, sender Sender) (proto.TaskAck, func()) {
				return rejectedAck(task.TaskID, "not used"), nil
			}), log, phase2)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				conn, acceptErr := listener.Accept()
				if acceptErr == nil {
					server.handleConn(ctx, conn)
				}
			}()
			conn := dialPhase2Loopback(t, listener.Addr().String())
			readLoopbackHello(t, conn)
			item := validPhase2Image(path)
			item.FieldsMask = test.field
			task := proto.Phase2Task{TaskID: "tcp-" + test.name, Stage: test.stage, Items: []proto.Phase2Item{item}}
			if err := conn.WriteFrame(proto.MsgPhase2Task, &task); err != nil {
				t.Fatal(err)
			}
			ack, feature := readPhase2FeatureStream(t, conn)
			if !ack.Accepted || ack.Reason != "accepted" || feature.FieldsDone != test.field || feature.Status != proto.StatusDone {
				t.Fatalf("ack=%#v feature=%#v", ack, feature)
			}
			if test.field == proto.FieldPHashParts {
				if !bytes.Equal(feature.PHashParts, test.payload) || len(feature.SobelHist) != 0 {
					t.Fatalf("stage-two payload=%#v", feature)
				}
			} else if !bytes.Equal(feature.SobelHist, test.payload) || len(feature.PHashParts) != 0 {
				t.Fatalf("stage-three payload=%#v", feature)
			}
			if submitted := pool.submittedSnapshot(); len(submitted) != 1 || submitted[0].ScreenStage != worker.ScreenStage(test.stage) {
				t.Fatalf("TCP submitted=%#v", submitted)
			}
			_ = conn.Close()
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("TCP server did not detach")
			}
		})
	}
}

func TestPhase2RealTCPLoopbackReconnectReplaysWithoutResubmission(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	pool := newPhase2FakePool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewPoolRouter(pool, log)
	path := `D:\loopback\image.jpg`
	phase2 := NewPhase2ManagerWithRuntime(
		"machine-a",
		&phase2CommittedFake{states: map[string]store.Phase2Committed{
			path: {MissingFields: proto.FieldPHashParts},
		}},
		pool,
		router,
		func(string) (int64, bool, error) { return 1, false, nil },
		log,
	)
	pool.onSubmit = func(job worker.JobMsg) {
		pool.results <- &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID,
			Path: job.Path, Kind: job.Kind, Phase: worker.Phase2,
			ScreenStage: job.ScreenStage, Source: job.Source,
			SHA512:     append([]byte(nil), job.KnownSHA...),
			FieldsDone: job.FieldsMask, PHashParts: []byte{1, 2, 3},
		}
	}
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-a"
	cfg.Proto.HeartbeatS = 60
	server := NewServer(
		cfg,
		scanHandlerFunc(func(
			task proto.ScanTask,
			sender Sender,
		) (proto.TaskAck, func()) {
			return rejectedAck(task.TaskID, "not used"), nil
		}),
		log,
		phase2,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveOne := func() <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			server.handleConn(ctx, connection)
		}()
		return done
	}
	task := proto.Phase2Task{
		TaskID: "phase2-loopback",
		Items:  []proto.Phase2Item{validPhase2Image(path)},
	}

	firstServerDone := serveOne()
	first := dialPhase2Loopback(t, listener.Addr().String())
	readLoopbackHello(t, first)
	if err := first.WriteFrame(proto.MsgPhase2Task, &task); err != nil {
		t.Fatal(err)
	}
	firstTypes, firstSeq, firstAck := readPhase2TaskStream(t, first)
	if firstAck.Reason != "accepted" ||
		len(firstSeq) != 1 || firstSeq[0] != 1 ||
		!containsMessageType(firstTypes, proto.MsgFeatureResult) ||
		!containsMessageType(firstTypes, proto.MsgTaskProgress) ||
		firstTypes[len(firstTypes)-1] != proto.MsgTaskDone {
		t.Fatalf("first stream types=%v seq=%v ack=%#v",
			firstTypes, firstSeq, firstAck)
	}
	_ = first.Close()
	select {
	case <-firstServerDone:
	case <-time.After(time.Second):
		t.Fatal("first TCP connection did not detach")
	}

	secondServerDone := serveOne()
	second := dialPhase2Loopback(t, listener.Addr().String())
	readLoopbackHello(t, second)
	if err := second.WriteFrame(proto.MsgPhase2Task, &task); err != nil {
		t.Fatal(err)
	}
	secondTypes, secondSeq, secondAck := readPhase2TaskStream(t, second)
	if secondAck.Reason != "already_done" ||
		len(secondSeq) != 1 || secondSeq[0] != 1 ||
		secondTypes[0] != proto.MsgTaskAck ||
		secondTypes[1] != proto.MsgFeatureResult ||
		secondTypes[len(secondTypes)-1] != proto.MsgTaskDone {
		t.Fatalf("replay stream types=%v seq=%v ack=%#v",
			secondTypes, secondSeq, secondAck)
	}
	if got := len(pool.submittedSnapshot()); got != 1 {
		t.Fatalf("reconnect submissions=%d, want 1", got)
	}
	_ = second.Close()
	select {
	case <-secondServerDone:
	case <-time.After(time.Second):
		t.Fatal("second TCP connection did not detach")
	}
	if err := phase2.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(pool.results)
}

func dialPhase2Loopback(t *testing.T, address string) *proto.Conn {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return proto.NewConn(connection)
}

func readLoopbackHello(t *testing.T, connection *proto.Conn) {
	t.Helper()
	msgType, body, err := connection.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	message, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := message.(*proto.Hello); !ok {
		t.Fatalf("first loopback message=%#v", message)
	}
}

func readPhase2TaskStream(
	t *testing.T,
	connection *proto.Conn,
) ([]uint8, []uint64, proto.TaskAck) {
	t.Helper()
	var types []uint8
	var sequences []uint64
	var ack proto.TaskAck
	for {
		msgType, body, err := connection.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, msgType)
		message, err := proto.Decode(msgType, body)
		if err != nil {
			t.Fatal(err)
		}
		switch value := message.(type) {
		case *proto.TaskAck:
			ack = *value
		case *proto.FeatureResult:
			sequences = append(sequences, value.Seq)
		case *proto.TaskDone:
			return types, sequences, ack
		}
	}
}

func readPhase2FeatureStream(t *testing.T, connection *proto.Conn) (proto.TaskAck, proto.FeatureItem) {
	t.Helper()
	var ack proto.TaskAck
	var feature proto.FeatureItem
	for {
		msgType, body, err := connection.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		message, err := proto.Decode(msgType, body)
		if err != nil {
			t.Fatal(err)
		}
		switch value := message.(type) {
		case *proto.TaskAck:
			ack = *value
		case *proto.FeatureResult:
			if len(value.Items) != 1 {
				t.Fatalf("FeatureResult items=%d, want 1", len(value.Items))
			}
			feature = value.Items[0]
		case *proto.TaskDone:
			return ack, feature
		}
	}
}

func containsMessageType(types []uint8, want uint8) bool {
	for _, current := range types {
		if current == want {
			return true
		}
	}
	return false
}
