package mockairlock

import "github.com/airlockrun/agentsdk/localruntime"

type FileStorage = localruntime.FileStorage

func NewFileStorage(root string) (*FileStorage, error) { return localruntime.NewFileStorage(root) }
