package migrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rudderlabs/rudder-server/config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/pathfinder"
	"github.com/rudderlabs/rudder-server/rruntime"
	"github.com/rudderlabs/rudder-server/services/filemanager"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
)

//Exporter is a handle to this object used in main.go
type Exporter struct {
	migrator     *Migrator
	pf           pathfinder.Pathfinder
	dumpQueues   map[string]chan []*jobsdb.JobT
	notifyQueues map[string]chan *jobsdb.MigrationEvent
}

var (
	dbReadBatchSize              int
	exportDoneCheckSleepDuration time.Duration
)

//Setup sets up exporter with underlying-migrator, pathfinder and initializes dumpQueus and notifyQueuss
func (exporter *Exporter) Setup(jobsDB *jobsdb.HandleT, pf pathfinder.Pathfinder) {
	logger.Infof("[[ %s-Export-Migrator ]] setup for jobsdb", jobsDB.GetTablePrefix())
	exporter.pf = pf
	exporter.dumpQueues = make(map[string]chan []*jobsdb.JobT)
	exporter.notifyQueues = make(map[string]chan *jobsdb.MigrationEvent)
	exporter.migrator = &Migrator{}
	exporter.migrator.Setup(jobsDB)
	exporter.migrator.jobsDB.SetupForExport()
	rruntime.Go(func() {
		exporter.export()
	})
}

func loadConfig() {
	dbReadBatchSize = config.GetInt("Migrator.dbReadBatchSize", 100000)
	exportDoneCheckSleepDuration = (config.GetDuration("Migrator.exportDoneCheckSleepDurationIns", time.Duration(20)) * time.Second)
}

func (exporter *Exporter) waitForExportDone() {
	logger.Infof("[[%s-Export-migrator ]] All jobs have been queried. Waiting for the same to be exported and acknowledged on notification", exporter.migrator.jobsDB.GetTablePrefix())
	isExportInProgress := true
	for ok := true; ok; ok = isExportInProgress {
		time.Sleep(exportDoneCheckSleepDuration)
		exportEvents := exporter.migrator.jobsDB.GetCheckpoints(jobsdb.ExportOp)
		isExportInProgress = false
		for _, exportEvent := range exportEvents {
			if exportEvent.Status == jobsdb.Exported {
				isExportInProgress = true
			}
		}
		if !isExportInProgress {
			isExportInProgress = exporter.migrator.jobsDB.IsMigrating()
		}
	}
}

func (exporter *Exporter) preExport() {
	logger.Infof("[[ %s-Export-migrator ]] Pre export", exporter.migrator.jobsDB.GetTablePrefix())
	exporter.migrator.jobsDB.PreExportCleanup()
}

func (exporter *Exporter) export() {

	if exporter.isExportDone() {
		return
	}

	exporter.preExport()

	rruntime.Go(func() {
		exporter.readFromCheckpointAndNotify()
	})

	logger.Infof("[[ %s-Export-migrator ]] export loop is starting", exporter.migrator.jobsDB.GetTablePrefix())
	for {
		toQuery := dbReadBatchSize

		jobList := exporter.migrator.jobsDB.GetNonMigratedAndMarkThemMigrating(toQuery)
		if len(jobList) == 0 {
			break
		}

		filteredData := exporter.filterByNode(jobList)
		exporter.delegateDump(filteredData)
	}

	exporter.waitForExportDone()

	exporter.postExport()
}

func (exporter *Exporter) filterByNode(jobList []*jobsdb.JobT) map[pathfinder.NodeMeta][]*jobsdb.JobT {
	logger.Infof("[[ %s-Export-migrator ]] Filtering a batch by destination nodes", exporter.migrator.jobsDB.GetTablePrefix())
	filteredData := make(map[pathfinder.NodeMeta][]*jobsdb.JobT)
	for _, job := range jobList {
		userID := exporter.migrator.jobsDB.GetUserID(job)
		nodeMeta := exporter.pf.GetNodeFromUserID(userID)
		filteredData[nodeMeta] = append(filteredData[nodeMeta], job)
	}
	return filteredData
}

func (exporter *Exporter) delegateDump(filteredData map[pathfinder.NodeMeta][]*jobsdb.JobT) {
	for nMeta, jobList := range filteredData {
		dumpQ, isNew := exporter.getDumpQForNode(nMeta.GetNodeID())
		if isNew {
			rruntime.Go(func() {
				exporter.writeToFileAndUpload(nMeta, dumpQ)
			})
		}
		dumpQ <- jobList
	}
}

func (exporter *Exporter) getDumpQForNode(nodeID string) (chan []*jobsdb.JobT, bool) {
	isNewChannel := false
	if _, ok := exporter.dumpQueues[nodeID]; !ok {
		dumpQ := make(chan []*jobsdb.JobT)
		exporter.dumpQueues[nodeID] = dumpQ
		isNewChannel = true
	}
	return exporter.dumpQueues[nodeID], isNewChannel
}

func (exporter *Exporter) writeToFileAndUpload(nMeta pathfinder.NodeMeta, ch chan []*jobsdb.JobT) {
	for {
		jobList := <-ch
		logger.Infof("[[ %s-Export-migrator ]] Received a batch for node:%s to be written to file and upload it", exporter.migrator.jobsDB.GetTablePrefix(), exporter.migrator.jobsDB.GetTablePrefix(), nMeta.GetNodeID())
		backupPathDirName := "/migrator-export/"
		tmpDirPath, err := misc.CreateTMPDIR()
		_ = err
		var jobState string
		var writeToFile bool
		if nMeta.GetNodeID() != misc.GetNodeID() {
			jobState = jobsdb.MigratedState
			writeToFile = true
		} else {
			jobState = jobsdb.WontMigrateState
			writeToFile = false
		}

		var statusList []*jobsdb.JobStatusT
		if writeToFile {
			path := fmt.Sprintf(`%v%s_%s_%s_%d_%d.gz`, tmpDirPath+backupPathDirName, exporter.migrator.jobsDB.GetTablePrefix(), misc.GetNodeID(), nMeta.GetNodeID(), jobList[0].JobID, len(jobList))

			err = os.MkdirAll(filepath.Dir(path), os.ModePerm)
			if err != nil {
				panic(err)
			}

			gzWriter, err := misc.CreateGZ(path)

			contentSlice := make([][]byte, len(jobList))
			for idx, job := range jobList {
				m, err := json.Marshal(job)
				if err != nil {
					logger.Error("Something went wrong in marshalling")
				}

				contentSlice[idx] = m
				statusList = append(statusList, jobsdb.BuildStatus(job, jobState))
			}

			logger.Info(nMeta, len(jobList))

			content := bytes.Join(contentSlice[:], []byte("\n"))
			gzWriter.Write(content)

			gzWriter.CloseGZ()
			file, err := os.Open(path)
			if err != nil {
				panic(err)
			}
			uploadOutput := exporter.upload(file, nMeta)
			//TODO: txn start
			migrationEvent := jobsdb.NewMigrationEvent("export", misc.GetNodeID(), nMeta.GetNodeID(), uploadOutput.Location, jobsdb.Exported, 0)
			migrationEvent.ID = exporter.migrator.jobsDB.Checkpoint(&migrationEvent)

			exporter.migrator.jobsDB.UpdateJobStatus(statusList, []string{}, []jobsdb.ParameterFilterT{})
			//TODO: txn end
			file.Close()

			os.Remove(path)
		} else {
			for _, job := range jobList {
				statusList = append(statusList, jobsdb.BuildStatus(job, jobState))
			}
			exporter.migrator.jobsDB.UpdateJobStatus(statusList, []string{}, []jobsdb.ParameterFilterT{})
		}
	}
}

func (exporter *Exporter) upload(file *os.File, nMeta pathfinder.NodeMeta) filemanager.UploadOutput {
	var (
		uploadOutput filemanager.UploadOutput
		err          error
	)
	for ok := true; ok; ok = (err != nil) {
		uploadOutput, err = exporter.migrator.fileManager.Upload(file)
	}
	logger.Infof("[[ %s-Export-migrator ]] Uploaded an export file to %s", exporter.migrator.jobsDB.GetTablePrefix(), uploadOutput.Location)
	return uploadOutput
}

func (exporter *Exporter) readFromCheckpointAndNotify() {
	notifiedCheckpoints := make(map[int64]*jobsdb.MigrationEvent)
	for {
		checkPoints := exporter.migrator.jobsDB.GetCheckpoints(jobsdb.ExportOp)
		for _, checkPoint := range checkPoints {
			_, found := notifiedCheckpoints[checkPoint.ID]
			if checkPoint.Status == jobsdb.Exported && !found {
				notifyQ, isNew := exporter.getNotifyQForNode(checkPoint.ToNode)
				if isNew {
					rruntime.Go(func() {
						exporter.notify(exporter.pf.GetNodeFromNodeID(checkPoint.ToNode), notifyQ)
					})
				}
				notifyQ <- checkPoint
				notifiedCheckpoints[checkPoint.ID] = checkPoint
			}
		}
	}
}

func (exporter *Exporter) getNotifyQForNode(nodeID string) (chan *jobsdb.MigrationEvent, bool) {
	isNewChannel := false
	if _, ok := exporter.notifyQueues[nodeID]; !ok {
		notifyQ := make(chan *jobsdb.MigrationEvent)
		exporter.notifyQueues[nodeID] = notifyQ
		isNewChannel = true
	}
	return exporter.notifyQueues[nodeID], isNewChannel
}

func (exporter *Exporter) notify(nMeta pathfinder.NodeMeta, notifyQ chan *jobsdb.MigrationEvent) {
	//TODO: Instead of this block, differentiate the events and not pass "All" events here
	if nMeta.GetNodeID() == "" {
		return
	}

	for {
		checkPoint := <-notifyQ
		statusCode := 0
		for ok := true; ok; ok = (statusCode != 200) {
			_, statusCode = misc.MakePostRequest(nMeta.GetNodeConnectionString(), exporter.migrator.getURI("/fileToImport"), checkPoint)
		}
		logger.Infof("[[ %s-Export-migrator ]] Notified destination node %s to download and import file from %s. Responded with statusCode: %d", exporter.migrator.jobsDB.GetTablePrefix(), checkPoint.ToNode, checkPoint.FileLocation, statusCode)
		checkPoint.Status = jobsdb.Notified
		exporter.migrator.jobsDB.Checkpoint(checkPoint)
	}
}

func (exporter *Exporter) postExport() {
	logger.Infof("[[ %s-Export-migrator ]] postExport", exporter.migrator.jobsDB.GetTablePrefix())
	exporter.migrator.jobsDB.PostExportCleanup()
	migrationEvent := jobsdb.NewMigrationEvent(jobsdb.ExportOp, misc.GetNodeID(), "All", jobsdb.Exported, jobsdb.Exported, 0)
	exporter.migrator.jobsDB.Checkpoint(&migrationEvent)
}

//ShouldExport tells if export should happen in migration
func (exporter *Exporter) isExportDone() bool {
	//Instead of this write a query to get a single checkpoint directly
	migrationStates := exporter.migrator.jobsDB.GetCheckpoints(jobsdb.ExportOp)
	if len(migrationStates) > 1 {
		lastExportMigrationState := migrationStates[len(migrationStates)-1]
		if lastExportMigrationState.ToNode == "All" && (lastExportMigrationState.Status == jobsdb.Exported || lastExportMigrationState.Status == jobsdb.Notified) {
			return true
		}
	}
	return false
}

//ExportStatusHandler returns true if export for this jobsdb is finished
func (exporter *Exporter) exportStatusHandler() bool {
	return exporter.isExportDone()
}
