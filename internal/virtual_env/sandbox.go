package virtual_env

import (
	"context"
	"github.com/bytedance/gopkg/util/logger"
	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino-ext/components/tool/commandline/sandbox"
)

func GetOperator(ctx context.Context) (commandline.Operator, error) {
	op, err := sandbox.NewDockerSandbox(ctx, &sandbox.Config{Image: "net-analyzer-v3:latest", VolumeBindings: map[string]string{
		"/home/hugo/ubuntu-mount": "/home/linuxbrew/pcaps",
	}})
	if err != nil {
		logger.Fatal(err)
	}
	// you should ensure that docker has been started before create a docker container
	err = op.Create(ctx)
	if err != nil {
		logger.Fatal(err)
	}

	return op, nil

}
