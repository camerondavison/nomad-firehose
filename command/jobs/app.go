package jobs

import (
	"encoding/json"
	"fmt"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	"github.com/seatgeek/nomad-firehose/sink"
	log "github.com/sirupsen/logrus"
)

// Firehose ...
type Firehose struct {
	lastChangeIndex  uint64
	lastChangeTimeCh chan interface{}
	nomadClient      *nomad.Client
	sink             sink.Sink
	stopCh           chan struct{}
}

// NewFirehose ...
func NewFirehose() (*Firehose, error) {
	nomadClient, err := nomad.NewClient(nomad.DefaultConfig())
	if err != nil {
		return nil, err
	}

	sink, err := sink.GetSink()
	if err != nil {
		return nil, err
	}

	return &Firehose{
		nomadClient:      nomadClient,
		sink:             sink,
		stopCh:           make(chan struct{}, 1),
		lastChangeTimeCh: make(chan interface{}, 1),
	}, nil
}

func (f *Firehose) Name() string {
	return "jobs"
}

func (f *Firehose) UpdateCh() <-chan interface{} {
	return f.lastChangeTimeCh
}

func (f *Firehose) SetRestoreValue(restoreValue interface{}) error {
	switch restoreValue.(type) {
	case int:
		f.lastChangeIndex = uint64(restoreValue.(int))
	case int64:
		f.lastChangeIndex = uint64(restoreValue.(int64))
	default:
		return fmt.Errorf("Unknown restore type '%T' with value '%+v'", restoreValue, restoreValue)
	}
	return nil
}

// Start the firehose
func (f *Firehose) Start() {
	go f.sink.Start()

	// watch for allocation changes
	go f.watch()

	// Save the last event time every 5s
	go f.persistLastChangeTime(5 * time.Second)

	// wait forever for a stop signal to happen
	select {
	case <-f.stopCh:
		return
	}
}

// Stop the firehose
func (f *Firehose) Stop() {
	close(f.stopCh)
	f.sink.Stop()
}

// Write the Last Change Time to Consul so if the process restarts,
// it will try to resume from where it left off, not emitting tons of double events for
// old events
func (f *Firehose) persistLastChangeTime(interval time.Duration) {
	ticker := time.NewTicker(interval)

	for {
		select {
		case <-f.stopCh:
			f.lastChangeTimeCh <- f.lastChangeIndex
			break
		case <-ticker.C:
			f.lastChangeTimeCh <- f.lastChangeIndex
		}
	}
}

// Publish an update from the firehose
func (f *Firehose) Publish(update *nomad.Job) {
	b, err := json.Marshal(update)
	if err != nil {
		log.Error(err)
	}

	f.sink.Put(b)
}

// Continously watch for changes to the allocation list and publish it as updates
func (f *Firehose) watch() {
	q := &nomad.QueryOptions{
		WaitIndex:  f.lastChangeIndex,
		WaitTime:   5 * time.Minute,
		AllowStale: true,
	}

	newMax := f.lastChangeIndex

	for {
		jobs, meta, err := f.nomadClient.Jobs().List(q)
		if err != nil {
			log.Errorf("Unable to fetch jobs: %s", err)
			time.Sleep(10 * time.Second)
			continue
		}

		remoteWaitIndex := meta.LastIndex
		localWaitIndex := q.WaitIndex

		// Only work if the WaitIndex have changed
		if remoteWaitIndex == localWaitIndex {
			log.Debugf("Jobs index is unchanged (%d == %d)", remoteWaitIndex, localWaitIndex)
			continue
		}

		log.Debugf("Jobs index is changed (%d <> %d)", remoteWaitIndex, localWaitIndex)

		// Iterate jobs and find events that have changed since last run
		for _, job := range jobs {
			if job.ModifyIndex <= f.lastChangeIndex {
				continue
			}

			if job.ModifyIndex > newMax {
				newMax = job.ModifyIndex
			}

			go func(jobID string) {
				fullJob, _, err := f.nomadClient.Jobs().Info(jobID, &nomad.QueryOptions{})
				if err != nil {
					log.Errorf("Could not read job %s: %s", jobID, err)
					return
				}

				f.Publish(fullJob)
			}(job.ID)
		}

		// Update WaitIndex and Last Change Time for next iteration
		q.WaitIndex = meta.LastIndex
		f.lastChangeIndex = newMax
	}
}
