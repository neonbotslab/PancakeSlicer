package main

import (
	"encoding/json"
	"errors"
	"strings"
)

type flowCalibrationMode uint8

const (
	flowCalibrationOff  flowCalibrationMode = 0
	flowCalibrationOn   flowCalibrationMode = 1
	flowCalibrationAuto flowCalibrationMode = 2
)

func parseFlowCalibrationMode(raw json.RawMessage, legacy bool) (flowCalibrationMode, error) {
	if raw == nil {
		if legacy {
			return flowCalibrationOn, nil
		}
		return flowCalibrationOff, nil
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		switch value {
		case "off":
			return flowCalibrationOff, nil
		case "auto":
			return flowCalibrationAuto, nil
		case "on":
			return flowCalibrationOn, nil
		}
	}
	return flowCalibrationOff, errors.New("flow_cali_mode must be off, auto or on")
}

func (p *PrinterConfig) supportsAutoFlowCalibration() bool {
	if p.Kind != "bambu-lan" {
		return false
	}
	serialModel := bambuSerialModel(p.Serial)
	if _, supported := bambuAutoFlowModels[strings.ToLower(serialModel)]; serialModel != "" && !supported {
		return false
	}
	model := p.model
	if model == "" {
		model = serialModel
	}
	model = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "bambu lab ")
	_, supported := bambuAutoFlowModels[model]
	return supported
}
