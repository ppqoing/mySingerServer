package wproc

import (
	"fmt"
	"io"

	"dedup/internal/worker"
)

func pumpSHAReply(conn *worker.IPCConn, query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
	if err := conn.Write(worker.MsgSHAQuery, query); err != nil {
		return nil, fmt.Errorf("write SHA query: %w", err)
	}
	for {
		envelope, err := conn.Read()
		if err != nil {
			return nil, fmt.Errorf("read SHA reply: %w", err)
		}
		if envelope.Type != worker.MsgSHAReply {
			if envelope.Type == worker.MsgShutdown {
				return nil, io.EOF
			}
			continue
		}
		reply, err := worker.DecodeBody[worker.SHAReplyMsg](envelope)
		if err != nil {
			return nil, err
		}
		if reply.JobID == query.JobID {
			return &reply, nil
		}
	}
}
