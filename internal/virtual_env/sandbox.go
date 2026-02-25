package virtual_env

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/bytedance/gopkg/util/logger"
	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino-ext/components/tool/commandline/sandbox"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// shadowDockerSandbox mirrors sandbox.DockerSandbox's memory layout
// so we can access the unexported docker client and container ID.
// MUST stay in sync with sandbox.DockerSandbox struct definition.
type shadowDockerSandbox struct {
	config      sandbox.Config
	client      *client.Client
	containerID string
}

// dockerInternals extracts the docker client and container ID from a DockerSandbox
// via unsafe pointer cast (the fields are unexported).
func dockerInternals(ds *sandbox.DockerSandbox) (*client.Client, string) {
	shadow := (*shadowDockerSandbox)(unsafe.Pointer(ds))
	return shadow.client, shadow.containerID
}

// CopyFileToContainer copies a local file into the running container at destPath.
// It uses the Docker CopyToContainer API with a tar archive, matching the
// pattern used by sandbox.WriteFile.
func CopyFileToContainer(ctx context.Context, ds *sandbox.DockerSandbox, localPath, destPath string) error {
	cli, containerID := dockerInternals(ds)
	if cli == nil || containerID == "" {
		return fmt.Errorf("sandbox not initialized")
	}

	// Read local file
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read local file %s: %w", localPath, err)
	}

	parentDir := filepath.Dir(destPath)
	if parentDir != "" && parentDir != "/" {
		_, err := ds.RunCommand(ctx, []string{"mkdir", "-p", parentDir})
		if err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	// Build tar archive containing the single file
	tarBuf := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuf)
	hdr := &tar.Header{
		Name: filepath.Base(destPath),
		Mode: 0644,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write tar content: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}

	// Upload to container
	destDir := filepath.Dir(destPath)
	if destDir == "" {
		destDir = "/"
	}
	err = cli.CopyToContainer(ctx, containerID, destDir, bytes.NewReader(tarBuf.Bytes()), container.CopyToContainerOptions{})
	if err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}

	return nil
}

// CopyReaderToContainer copies from an io.Reader into the container at destPath.
// Useful when the data is already in memory or comes from a stream.
func CopyReaderToContainer(ctx context.Context, ds *sandbox.DockerSandbox, reader io.Reader, destPath string, size int64) error {
	cli, containerID := dockerInternals(ds)
	if cli == nil || containerID == "" {
		return fmt.Errorf("sandbox not initialized")
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read data: %w", err)
	}

	tarBuf := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuf)
	hdr := &tar.Header{
		Name: filepath.Base(destPath),
		Mode: 0644,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write tar content: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}

	destDir := filepath.Dir(destPath)
	if destDir == "" {
		destDir = "/"
	}
	return cli.CopyToContainer(ctx, containerID, destDir, bytes.NewReader(tarBuf.Bytes()), container.CopyToContainerOptions{})
}

func GetOperator(ctx context.Context) (commandline.Operator, error) {
	op, err := sandbox.NewDockerSandbox(ctx, &sandbox.Config{Image: "net-analyzer-v3:latest"})
	if err != nil {
		logger.Fatal(err)
	}
	err = op.Create(ctx)
	if err != nil {
		logger.Fatal(err)
	}

	return op, nil
}
