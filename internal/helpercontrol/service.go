package helpercontrol

import (
	"context"
	"fmt"
	"net"

	"dedup/internal/nodectl"
)

type Service struct {
	provider nodectl.StatusProvider
	shutdown nodectl.ShutdownFunc
	listen   func(string) (net.Listener, error)
}

func New(provider nodectl.StatusProvider, shutdown nodectl.ShutdownFunc) *Service {
	return &Service{provider: provider, shutdown: shutdown, listen: nodectl.Listen}
}

func (s *Service) Run(ctx context.Context) error {
	listener, err := s.listen(nodectl.HelperPipeName())
	if err != nil {
		return fmt.Errorf("listen Helper control pipe: %w", err)
	}
	defer listener.Close()
	return nodectl.Serve(ctx, listener, s.provider, s.shutdown)
}
