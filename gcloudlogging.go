package GCloudLoggingOutput

import (
	"errors"
	"log"
	"net/http"
	"time"
	"fmt"

	"github.com/mozilla-services/heka/pipeline"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/logging/v1beta3"
	"google.golang.org/cloud/compute/metadata"
)

//CloudLoggingConfig ...
type CloudLoggingConfig struct {
	ProjectID     string `toml:"project_id"`
	LogName       string `toml:"logname"`
	Zone          string `toml:"zone"`
	ResourceID    string `toml:"resource_id"`
	FlushInterval uint32 `toml:"flush_interval"`
	FlushCount    int    `toml:"flush_count"`
}

//CloudLoggingOutput ...
type CloudLoggingOutput struct {
	conf       *CloudLoggingConfig
	client     *http.Client
	service    *logging.Service
	backChan   chan []*logging.LogEntry
	batchChan  chan LogBatch // Chan to pass completed batches
	outputExit chan error
	or         pipeline.OutputRunner
}

//LogBatch ...
type LogBatch struct {
	name  string
	count int64
	batch []*logging.LogEntry
}

//ConfigStruct ...
func (clo *CloudLoggingOutput) ConfigStruct() interface{} {
	return &CloudLoggingConfig{Zone: "asia-east1-a", ResourceID: "0", LogName: "projects/pristine-abacus-90205/logs/syslog"}
}

//Init ...
func (clo *CloudLoggingOutput) Init(config interface{}) (err error) {
	log.Print("Init")

	clo.conf = config.(*CloudLoggingConfig)

	if metadata.OnGCE() {
		if clo.conf.ProjectID == "" {
			if clo.conf.ProjectID, err = metadata.ProjectID(); err != nil {
				return
			}
		}
		if clo.conf.ResourceID == "" {
			if clo.conf.ResourceID, err = metadata.InstanceID(); err != nil {
				return
			}
		}
		if clo.conf.Zone == "" {
			if clo.conf.Zone, err = metadata.Get("instance/zone"); err != nil {
				return
			}
		}
	}
	if clo.conf.ProjectID == "" {
		return errors.New("ProjectID cannot be blank")
	}

	clo.batchChan = make(chan LogBatch)
	clo.backChan = make(chan []*logging.LogEntry, 2)
	clo.outputExit = make(chan error)
	if clo.client, err = google.DefaultClient(oauth2.NoContext,
		logging.CloudPlatformScope); err != nil {
		return
	}
	if clo.service, err = logging.New(clo.client); err != nil {
		return
	}
	_, err = clo.service.Projects.LogServices.List(clo.conf.ProjectID).Do()
	if err != nil {
		log.Print("Init CloudLoggingOutput Error: ", err)
	}
	return
}

// Run ...
func (clo *CloudLoggingOutput) Run(or pipeline.OutputRunner, h pipeline.PluginHelper) (err error) {
	var (
		pack       *pipeline.PipelinePack
		e          error
		k          string
		m          *logging.LogEntry
		exist      bool
		ok         = true
		inChan     = or.InChan()
		groupBatch = make(map[string]*LogBatch)
		outBatch   *LogBatch
		ticker     = time.Tick(time.Duration(clo.conf.FlushInterval) * time.Millisecond)
	)
	clo.or = or
	go clo.committer()
	for ok {
		select {
		case pack, ok = <-inChan:
			// Closed inChan => we're shutting down, flush data.
			if !ok {
				clo.sendGroupBatch(groupBatch)
				close(clo.batchChan)
				<-clo.outputExit
				break
			}

			k, m, e = clo.Encode(pack)
			pack.Recycle(fmt.Errorf("can't encode: %s", e))
			if e != nil {
				or.LogError(e)
				continue
			}

			if k != "" && m != nil {
				outBatch, exist = groupBatch[k]
				if !exist {
					outBatch = &LogBatch{count: 0, batch: make([]*logging.LogEntry, 0, 100), name: k}
					groupBatch[k] = outBatch
				}

				outBatch.batch = append(outBatch.batch, m)
				if outBatch.count++; clo.CheckFlush(int(outBatch.count), len(outBatch.batch)) {
					if len(outBatch.batch) > 0 {
						outBatch.batch = clo.sendBatch(k, outBatch.batch, outBatch.count)
						outBatch.count = 0
					}
				}
			}
		case <-ticker:
			clo.sendGroupBatch(groupBatch)
		case err = <-clo.outputExit:
			ok = false
		}
	}
	return
}

func (clo *CloudLoggingOutput) committer() {
	clo.backChan <- make([]*logging.LogEntry, 0, 100)

	for b := range clo.batchChan {
		if err := clo.SendRecord(b.name, b.batch); err != nil {
			clo.or.LogError(err)
		}
		b.batch = b.batch[:0]
		clo.backChan <- b.batch
	}
	clo.outputExit <- nil
}

//SendRecord ...
func (clo *CloudLoggingOutput) SendRecord(name string, entries []*logging.LogEntry) (err error) {
	
	log.Print("send record: ")

	labels := map[string]string{
		"compute.googleapis.com/resource_type": "instance",
		"compute.googleapis.com/resource_id":   clo.conf.ResourceID,
	}
	e := &logging.WriteLogEntriesRequest{CommonLabels: labels, Entries: entries}

	_, err = clo.service.Projects.Logs.Entries.Write(clo.conf.ProjectID, name, e).Do()
	if err != nil {
		log.Print("Write Log Error: ", err)
	}
	return
}

func (clo *CloudLoggingOutput) sendBatch(name string, entries []*logging.LogEntry, count int64) (nextBatch []*logging.LogEntry) {
	// This will block until the other side is ready to accept
	// this batch, so we can't get too far ahead.
	log.Print("send batch ")
	b := LogBatch{
		count: count,
		batch: entries,
		name:  name,
	}
	clo.batchChan <- b
	nextBatch = <-clo.backChan
	return nextBatch
}

func (clo *CloudLoggingOutput) sendGroupBatch(batch map[string]*LogBatch) {
	log.Print("sendingGroupBatch")
	for _, b := range batch {
		if len(b.batch) > 0 {
			b.batch = clo.sendBatch(b.name, b.batch, b.count)
			b.count = 0
		}
	}
	return
}

//CheckFlush ...
func (clo *CloudLoggingOutput) CheckFlush(count int, length int) bool {
	if count >= clo.conf.FlushCount {
		return true
	}
	return false
}

var severityMapping = map[int32]string{
	0: "EMERGENCY",
	1: "ALERT",
	2: "CRITICAL",
	3: "ERROR",
	4: "WARNING",
	5: "NOTICE",
	6: "INFO",
	7: "DEBUG",
	8: "DEFAULT",
}

func getSeverity(is int32) string {
	if is > 8 {
		is = 8
	}
	return severityMapping[is]
}

//Encode ...
func (clo *CloudLoggingOutput) Encode(pack *pipeline.PipelinePack) (name string, entry *logging.LogEntry, err error) {
	message := pack.Message
	labels := map[string]string{}
	if *message.Hostname != "" {
		labels["compute.googleapis.com/resource_id"] = *message.Hostname
	}

	if *message.Logger != "" {
		labels["logger"] = *message.Logger
	}

	if *message.EnvVersion != "" {
		labels["envVersion"] = "error test"
	}

	if *message.Type != "" {
		name = *message.Type
	} else {
		name = clo.conf.LogName
	}
	uuid := string(message.Uuid)
	labels["uuid"] = uuid

	log.Print("name is: ", name)
	log.Print("pl: ", message.GetPayload())

	for _, v := range message.Fields {
		if v != nil {
			fiedlName :=  v.GetName()
			if str, ok := message.GetFieldValue(fiedlName); ok {
			   labels[fiedlName] = str.(string)
			} 		
		}
	}

	meta := &logging.LogEntryMetadata{
		Timestamp:   getTimestamp(*message.Timestamp),
		Severity:    getSeverity(*message.Severity),
		ProjectId:   clo.conf.ProjectID,
		ServiceName: "compute.googleapis.com",
		Zone:        clo.conf.Zone,
		Labels:      labels,
	}
	log.Print(meta)

	entry = &logging.LogEntry{Metadata: meta, TextPayload: message.GetPayload()}
	return
}

func init() {
	pipeline.RegisterPlugin("CloudLoggingOutput", func() interface{} {
		return new(CloudLoggingOutput)
	})
}

func getTimestamp(t int64) string {
	return time.Unix(0, t).UTC().Format(time.RFC3339)
}
