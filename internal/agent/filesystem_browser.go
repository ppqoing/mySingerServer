package agent

import (
	"context"

	"dedup/internal/proto"
)

type FilesystemBrowser interface {
	Browse(context.Context, proto.FilesystemBrowseRequest) proto.FilesystemBrowseResponse
}
