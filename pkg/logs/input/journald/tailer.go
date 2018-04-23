// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

package journald

import (
	log "github.com/cihub/seelog"

	"github.com/DataDog/datadog-agent/pkg/logs/config"
	"github.com/DataDog/datadog-agent/pkg/logs/message"
)

// JournalConfig enables to configure the tailer:
// - Units: the units to filter on
// - Path: the path of the journal
type JournalConfig struct {
	Units []string
	Path  string
}

// NewTailer returns a new tailer.
func NewTailer(config JournalConfig, source *config.LogSource, outputChan chan message.Message) *Tailer {
	return &Tailer{
		config:     config,
		source:     source,
		outputChan: outputChan,
		stop:       make(chan struct{}, 1),
		done:       make(chan struct{}, 1),
	}
}

// Identifier returns the unique identifier of the current journal being tailed.
func (t *Tailer) Identifier() string {
	if t.config.Path != "" {
		return "journald:" + t.config.Path
	}
	return "journald:default"
}

// Start starts tailing the journal from a given offset.
func (t *Tailer) Start(cursor string) error {
	if err := t.setup(); err != nil {
		return err
	}
	if err := t.seek(cursor); err != nil {
		return err
	}
	log.Info("Start tailing journal")
	go t.tail()
	return nil
}

// Stop stops the tailer
func (t *Tailer) Stop() {
	log.Info("Stop tailing journal")
	t.stop <- struct{}{}
	<-t.done
}
