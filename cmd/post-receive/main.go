package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"cuelang.org/go/cue/cuecontext"
)

type CIConfig struct {
	Tekton struct {
		Pipeline string `json:"pipeline"`
	} `json:"tekton"`
}

type TektonPayload struct {
	Repo           string `json:"repo"`
	Branch         string `json:"branch"`
	Commit         string `json:"commit"`
	Message        string `json:"message"`
	Author         string `json:"author"`
	Email          string `json:"email"`
	TektonPipeline string `json:"tektonPipeline,omitempty"`
}

type TektonResponse struct {
	EventListenerUID string `json:"eventListenerUID"`
	EventID          string `json:"eventID"`
}

func main() {
	ctx := context.Background()
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{}))
	err := run(ctx, lg)
	if err != nil {
		lg.LogAttrs(ctx, slog.LevelError, "failed", slog.String("error", err.Error()))
	}
}

func run(ctx context.Context, lg *slog.Logger) error {
	n, _ := strconv.ParseInt(os.Getenv("GIT_PUSH_OPTION_COUNT"), 10, 64)
	pushOptions := make(map[string]string)
	for i := 0; i < int(n); i++ {
		k, v, _ := strings.Cut(os.Getenv(fmt.Sprintf("GIT_PUSH_OPTION_%d", i)), "=")
		pushOptions[k] = v
	}

	if _, ok := pushOptions["ci.skip"]; ok {
		lg.LogAttrs(ctx, slog.LevelInfo, "skipping ci", slog.String("push.option", "ci.skip"))
		return nil
	}

	dir, err := os.Getwd()
	if err != nil {
		lg.LogAttrs(ctx, slog.LevelError, "failed to get working directory", slog.String("error", err.Error()))
		return err
	}
	repoName := strings.TrimSuffix(filepath.Base(dir), ".git")

	var oldRev, newRev, refName string
	_, err = fmt.Scanln(&oldRev, &newRev, &refName)
	if err != nil {
		lg.LogAttrs(ctx, slog.LevelError, "failed to scan post-receive input", slog.String("error", err.Error()))
		return err
	}

	commit := newRev
	branch := mustExecGit(`rev-parse`, `--abbrev-ref`, refName)
	message := mustExecGit(`log`, `-1`, `HEAD`, `--format=%B`, `--`)
	author := mustExecGit(`log`, `-1`, `HEAD`, `--format=%an`, `--`)
	email := mustExecGit(`log`, `-1`, `HEAD`, `--format=%ae`, `--`)
	ciConfig, err := readCIConfig(newRev)
	if err != nil {
		lg.LogAttrs(ctx, slog.LevelWarn, "failed to get ci.cue", slog.String("error", err.Error()))
	}

	// tekton
	tektonResponse, err := func() (string, error) {
		endpoint := os.Getenv("TEKTON_TRIGGERS_ENDPOINT")
		if endpoint == "" {
			return "", fmt.Errorf("no TEKTON_TRIGGERS_ENDPOINT provided")
		}

		payload := TektonPayload{
			Repo:           repoName,
			Branch:         branch,
			Commit:         commit,
			Message:        message,
			Author:         author,
			Email:          email,
			TektonPipeline: ciConfig.Tekton.Pipeline,
		}

		b, err := json.Marshal(payload)
		if err != nil {
			lg.LogAttrs(ctx, slog.LevelError, "failed to marshal payload", slog.String("error", err.Error()))
			return "", err
		}
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(b))
		if err != nil {
			lg.LogAttrs(ctx, slog.LevelError, "failed to create request", slog.String("error", err.Error()))
			return "", err
		}
		req.Header.Set("content-type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			lg.LogAttrs(ctx, slog.LevelError, "failed to send request to tekton", slog.String("error", err.Error()))
			return "", err
		}
		if res.StatusCode < 200 || res.StatusCode > 299 {
			io.Copy(os.Stdout, res.Body)
			fmt.Println()
			log.Println("body", string(b))
			log.Fatalln("unexpected response from tekton", res.Status)
		}
		var response TektonResponse
		err = json.NewDecoder(res.Body).Decode(&response)
		if err != nil {
			lg.LogAttrs(ctx, slog.LevelError, "failed to read response", slog.String("error", err.Error()))
			return "", err
		}
		lg.LogAttrs(ctx, slog.LevelDebug, "got response", slog.String("eventlistener_uid", response.EventListenerUID), slog.String("event_id", response.EventID))

		return "event-id:\t" + response.EventID, nil
	}()
	if err != nil {
		lg.LogAttrs(ctx, slog.LevelError, "send to tekton", slog.String("error", err.Error()))
	}

	fmt.Println()
	fmt.Printf("\ttekton: %s\n", tektonResponse)
	fmt.Println()
	return nil
}

func mustExecGit(args ...string) string {
	b, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		log.Println("output", string(b))
		log.Fatalln("run git", args, err)
	}
	return strings.TrimSpace(string(b))
}

func readCIConfig(rev string) (CIConfig, error) {
	b, err := exec.Command("git", "show", rev+":"+"ci.cue").CombinedOutput()
	if err != nil {
		return CIConfig{}, fmt.Errorf("git show %s:ci.cue: %w", rev, err)
	}
	var ciConfig CIConfig
	err = cuecontext.New().CompileBytes(b).Decode(&ciConfig)
	if err != nil {
		return CIConfig{}, fmt.Errorf("cue decode ci.cue: %w", err)
	}
	return ciConfig, nil
}
