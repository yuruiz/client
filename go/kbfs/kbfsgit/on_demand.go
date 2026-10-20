// Copyright 2026 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package kbfsgit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/keybase/client/go/kbfs/libcontext"
	"github.com/keybase/client/go/kbfs/libkbfs"
	"github.com/keybase/client/go/logger"
)

// StartOnDemand handles protocol negotiation without opening a session. initialize
// is called only after a repository operation arrives. Its cleanup runs after all
// repository work stops, including when initialization or the operation fails.
// The caller owns input and must close it when cancelling a blocked read.
func StartOnDemand(ctx context.Context, options StartOptions,
	input io.Reader, output, errput io.Writer,
	initialize func(context.Context) (libkbfs.Config, func() error, error),
) (retErr error) {
	if _, _, _, err := ParseRepo(options.Repo); err != nil {
		return err
	}
	reader := bufio.NewReader(input)
	negotiation := &runner{
		log: logger.NewNull(), output: output, verbosity: 1, progress: true,
	}
	first, err := negotiate(ctx, reader, negotiation, options.LFS)
	if err != nil || first == "" {
		return err
	}
	ctx, err = libcontext.NewContextWithCancellationDelayer(
		libcontext.NewContextReplayable(ctx, func(ctx context.Context) context.Context { return ctx }))
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, libcontext.CleanupCancellationDelayer(ctx))
	}()
	config, cleanup, err := initialize(ctx)
	if cleanup != nil {
		defer func() { retErr = errors.Join(retErr, cleanup()) }()
	}
	if err != nil {
		return err
	}
	if config == nil {
		return errors.New("repository initializer returned no config")
	}
	processType := processGit
	if options.LFS {
		processType = processLFS
	}
	r, err := newRunnerWithType(ctx, config, options.Remote, options.Repo,
		options.GitDir, io.MultiReader(strings.NewReader(first), reader),
		output, errput, processType)
	if err != nil {
		return err
	}
	r.verbosity, r.progress, r.cloning =
		negotiation.verbosity, negotiation.progress, negotiation.cloning
	return r.processCommands(ctx)
}

func negotiate(ctx context.Context, reader *bufio.Reader, r *runner, lfs bool) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) && line == "" {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("reading remote-helper command: %w", err)
		}
		if lfs {
			var req lfsRequest
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				return "", err
			}
			switch req.Event {
			case gitLFSInitEvent:
				if _, err := io.WriteString(r.output, "{}\n"); err != nil {
					return "", err
				}
			case gitLFSTerminateEvent:
				return "", nil
			case gitLFSUploadEvent, gitLFSDownloadEvent:
				return line, nil
			default:
				return "", fmt.Errorf("unsupported LFS event %q", req.Event)
			}
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			return "", nil
		}
		switch parts[0] {
		case gitCmdCapabilities:
			if len(parts) != 1 {
				return "", errors.New("capabilities takes no arguments")
			}
			if err := r.handleCapabilities(); err != nil {
				return "", err
			}
		case gitCmdOption:
			if err := r.handleOption(ctx, parts[1:]); err != nil {
				return "", err
			}
		case gitCmdList:
			if len(parts) != 1 && (len(parts) != 2 || parts[1] != "for-push") {
				return "", errors.New("expected list or list for-push")
			}
			return line, nil
		case gitCmdFetch:
			if len(parts) != 3 {
				return "", errors.New("expected fetch <sha1> <name>")
			}
			return line, nil
		case gitCmdPush:
			if len(parts) != 2 {
				return "", errors.New("expected push <refspec>")
			}
			return line, nil
		default:
			return "", fmt.Errorf("unsupported remote-helper command %q", parts[0])
		}
	}
}
