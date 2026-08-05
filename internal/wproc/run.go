//go:build windows

package wproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"dedup/internal/worker"
	"dedup/internal/wproc/videocore"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func Run(pipeName string, index int) int {
	suppressWERDialogs()
	cfg, err := ConfigFromEnv()
	if err != nil {
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		return 2
	}
	cfg.FFprobePath, cfg.FFmpegPath, err = resolveFFmpegTools(cfg, executable)
	if err != nil {
		return 2
	}
	timeout := 10 * time.Second
	conn, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		return 2
	}
	defer conn.Close()
	return serve(conn, index, cfg, pipelineDeps{})
}

func serve(conn net.Conn, index int, cfg Config, deps pipelineDeps) int {
	ipc := worker.NewIPCConnWithMax(conn, cfg.IPCMaxFrameBytes)
	runtimeFn := deps.runtime
	if runtimeFn == nil && deps.session != nil {
		runtimeFn = deps.session.runtime
	}
	if runtimeFn == nil {
		runtimeFn = videocore.Runtime
	}
	runtimeInfo, err := runtimeFn()
	if err != nil {
		return 2
	}
	components := make([]worker.RuntimeComponent, 0, len(runtimeInfo.Components))
	for _, component := range runtimeInfo.Components {
		components = append(components, worker.RuntimeComponent{
			Name:           component.Name,
			BuildVersion:   ffmpegVersionString(component.HeaderVersion),
			RuntimeVersion: ffmpegVersionString(component.RuntimeVersion),
			BuildMajor:     component.HeaderVersion >> 16,
			RuntimeMajor:   component.RuntimeVersion >> 16,
		})
	}
	if err := ipc.Write(worker.MsgReady, worker.ReadyMsg{
		PID: os.Getpid(), WorkerIndex: index,
		IPCVersion: worker.IPCCompatibilityVersion, DLLVersion: runtimeInfo.Version,
		VideoCoreABI: runtimeInfo.ABI, VideoCoreVersion: runtimeInfo.Version,
		FFmpegComponents: components,
	}); err != nil {
		return 2
	}

	videoOverride := deps.video
	phase2Override := deps.phase2
	sessionOverride := deps.session
	useSessionPipeline := sessionOverride != nil || (deps.open == nil && videoOverride == nil && phase2Override == nil)
	var sessionDeps sessionPipelineDeps
	if sessionOverride != nil {
		sessionDeps = *sessionOverride
		if sessionDeps.query == nil {
			sessionDeps.query = func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
				return pumpSHAReply(ipc, query)
			}
		}
	} else if useSessionPipeline {
		sessionDeps = defaultSessionPipelineDeps(func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
			return pumpSHAReply(ipc, query)
		})
	}
	if !useSessionPipeline && deps.open == nil {
		deps = defaultPipelineDeps(func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
			return pumpSHAReply(ipc, query)
		})
	} else if !useSessionPipeline && deps.query == nil {
		deps.query = func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
			return pumpSHAReply(ipc, query)
		}
	}
	var videoDeps videoPipelineDeps
	if videoOverride == nil && !useSessionPipeline {
		videoDeps = defaultVideoPipelineDeps(func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
			return pumpSHAReply(ipc, query)
		})
	} else if !useSessionPipeline {
		videoDeps = *videoOverride
		if videoDeps.query == nil {
			videoDeps.query = func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
				return pumpSHAReply(ipc, query)
			}
		}
	}
	var phase2Deps phase2PipelineDeps
	if phase2Override == nil && !useSessionPipeline {
		phase2Deps = defaultPhase2PipelineDeps()
	} else if !useSessionPipeline {
		phase2Deps = *phase2Override
	}

	for {
		envelope, err := ipc.Read()
		if err != nil {
			if errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return 0
			}
			return 2
		}
		switch envelope.Type {
		case worker.MsgShutdown:
			return 0
		case worker.MsgJob:
			job, err := worker.DecodeBody[worker.JobMsg](envelope)
			if err != nil {
				return 2
			}
			if cfg.CrashInjection {
				if strings.Contains(job.Path, "__crash__") {
					mediacoreDebugCrash()
				}
				if strings.Contains(job.Path, "__hang__") {
					mediacoreDebugSleep(10 * time.Minute)
				}
			}
			var result *worker.JobResultMsg
			if useSessionPipeline {
				if job.Phase != worker.Phase1 && job.Phase != worker.Phase2 {
					result = invalidDispatchResult(&job, "phase", "unsupported worker phase")
				} else if job.Kind != worker.MediaImage && job.Kind != worker.MediaVideo {
					result = invalidDispatchResult(&job, "kind", "unsupported media kind")
				} else {
					result, err = processMediaWithDeps(context.Background(), cfg, &job, sessionDeps)
				}
			} else {
				switch job.Phase {
				case worker.Phase1:
					switch job.Kind {
					case worker.MediaImage:
						result, err = processImageWithDeps(cfg, &job, deps)
					case worker.MediaVideo:
						result, err = processVideoWithDeps(context.Background(), cfg, &job, videoDeps)
					default:
						result = invalidDispatchResult(&job, "kind", "unsupported media kind")
					}
				case worker.Phase2:
					if job.Kind != worker.MediaImage && job.Kind != worker.MediaVideo {
						result = invalidDispatchResult(&job, "kind", "unsupported media kind")
					} else {
						result, err = processPhase2WithDeps(context.Background(), cfg, &job, phase2Deps)
					}
				default:
					result = invalidDispatchResult(&job, "phase", "unsupported worker phase")
				}
			}
			if err != nil {
				return 2
			}
			if err := ipc.Write(worker.MsgResult, result); err != nil {
				return 2
			}
		default:
			return 2
		}
	}
}

func ffmpegVersionString(version uint32) string {
	return fmt.Sprintf("%d.%d.%d", version>>16, version>>8&0xff, version&0xff)
}

func invalidDispatchResult(job *worker.JobMsg, stage, message string) *worker.JobResultMsg {
	return &worker.JobResultMsg{
		JobID:      job.JobID,
		ScanTaskID: job.ScanTaskID,
		Phase:      job.Phase,
		Path:       job.Path,
		Kind:       job.Kind,
		SHA512:     append([]byte(nil), job.KnownSHA...),
		Errors: []worker.FieldError{{
			Field: 0,
			Stage: stage,
			Msg:   message,
		}},
	}
}

func suppressWERDialogs() {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	const (
		semFailCriticalErrors = 0x0001
		semNoGPFaultErrorBox  = 0x0002
	)
	_, _, _ = kernel32.NewProc("SetErrorMode").Call(semFailCriticalErrors | semNoGPFaultErrorBox)
}
