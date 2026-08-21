package daemon

import (
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func deviceFunctions(dev *store.Device) model.DeviceFunctions {
	return model.DeviceFunctionsOf(dev.Functions, dev.Role)
}

func renderDevice(dev *store.Device) model.Device {
	return dev.ModelDevice()
}
