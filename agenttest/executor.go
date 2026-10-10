package agenttest

import (
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/localruntime"
)

type ExecutorConfig = localruntime.ExecutorConfig

func Executor(config ExecutorConfig) (chatruntime.ExecutorFactory, error) {
	return localruntime.Executor(config)
}
