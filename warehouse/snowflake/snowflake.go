package snowflake

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/rudderlabs/rudder-server/config"
	"github.com/rudderlabs/rudder-server/rruntime"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
	uuid "github.com/satori/go.uuid"
	snowflake "github.com/snowflakedb/gosnowflake" //blank comment
)

var (
	warehouseUploadsTable string
	stagingTablePrefix    string
	maxParallelLoads      int
)

type HandleT struct {
	DbHandle      *sql.DB
	Db            *sql.DB
	Namespace     string
	CurrentSchema map[string]map[string]string
	Warehouse     warehouseutils.WarehouseT
	Upload        warehouseutils.UploadT
}

var dataTypesMap = map[string]string{
	"boolean":  "boolean",
	"int":      "number",
	"bigint":   "number",
	"float":    "double precision",
	"string":   "varchar",
	"datetime": "timestamp",
}

var primaryKeyMap = map[string]string{
	"users":      "ID",
	"identifies": "ID",
}

func columnsWithDataTypes(columns map[string]string, prefix string) string {
	arr := []string{}
	for name, dataType := range columns {
		arr = append(arr, fmt.Sprintf(`"%s%s" %s`, prefix, strings.ToUpper(name), dataTypesMap[dataType]))
	}
	return strings.Join(arr[:], ",")
}

func (sf *HandleT) createTable(name string, columns map[string]string) (err error) {
	sqlStatement := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%s" ( %v )`, strings.ToUpper(name), columnsWithDataTypes(columns, ""))
	logger.Infof("Creating table in snowflake for SF:%s : %v", sf.Warehouse.Destination.ID, sqlStatement)
	_, err = sf.Db.Exec(sqlStatement)
	return
}

func (sf *HandleT) tableExists(tableName string) (exists bool, err error) {
	sqlStatement := fmt.Sprintf(`SELECT EXISTS ( SELECT 1
   								 FROM   information_schema.tables
   								 WHERE  table_schema = '%s'
   								 AND    table_name = '%s'
								   )`, sf.Namespace, tableName)
	err = sf.Db.QueryRow(sqlStatement).Scan(&exists)
	return
}

func (sf *HandleT) addColumn(tableName string, columnName string, columnType string) (err error) {
	sqlStatement := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN "%s" %s`, strings.ToUpper(tableName), strings.ToUpper(columnName), dataTypesMap[columnType])
	logger.Infof("Adding column in snowflake for SF:%s : %v", sf.Warehouse.Destination.ID, sqlStatement)
	_, err = sf.Db.Exec(sqlStatement)
	return
}

func (sf *HandleT) createSchema() (err error) {
	sqlStatement := fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS "%s"`, sf.Namespace)
	logger.Infof("Creating schemaname in snowflake for SF:%s : %v", sf.Warehouse.Destination.ID, sqlStatement)
	_, err = sf.Db.Exec(sqlStatement)
	return
}

func (sf *HandleT) updateSchema() (updatedSchema map[string]map[string]string, err error) {
	diff := warehouseutils.GetSchemaDiff(sf.CurrentSchema, sf.Upload.Schema)
	updatedSchema = diff.UpdatedSchema
	if len(sf.CurrentSchema) == 0 {
		err = sf.createSchema()
		if err != nil {
			return nil, err
		}
	}

	sqlStatement := fmt.Sprintf(`USE SCHEMA "%s"`, sf.Namespace)
	_, err = sf.Db.Exec(sqlStatement)
	if err != nil {
		return nil, err
	}

	processedTables := make(map[string]bool)
	for _, tableName := range diff.Tables {
		tableExists, err := sf.tableExists(tableName)
		if err != nil {
			return nil, err
		}
		if !tableExists {
			err = sf.createTable(fmt.Sprintf(`%s`, tableName), diff.ColumnMaps[tableName])
			if err != nil {
				return nil, err
			}
			processedTables[tableName] = true
		}
	}
	for tableName, columnMap := range diff.ColumnMaps {
		// skip adding columns when table didn't exist previously and was created in the prev statement
		// this to make sure all columns in the the columnMap exists in the table in snowflake
		if _, ok := processedTables[tableName]; ok {
			continue
		}
		if len(columnMap) > 0 {
			for columnName, columnType := range columnMap {
				err := sf.addColumn(tableName, columnName, columnType)
				if !checkAndIgnoreAlreadyExistError(err) {
					return nil, err
				}
			}
		}
	}
	return
}

func checkAndIgnoreAlreadyExistError(err error) bool {
	if err != nil {
		if e, ok := err.(*snowflake.SnowflakeError); ok {
			if e.SQLState == "42601" {
				return true
			}
		}
		return false
	}
	return true
}

func (sf *HandleT) loadTable(tableName string, columnMap map[string]string, accessKeyID, accessKey string) (err error) {
	status, err := warehouseutils.GetTableUploadStatus(sf.Upload.ID, tableName, sf.DbHandle)
	if status == warehouseutils.ExportedDataState {
		logger.Infof("SF: Skipping load for table:%s as it has been succesfully loaded earlier", tableName)
		return
	}
	logger.Infof("SF: Starting load for table:%s\n", tableName)
	warehouseutils.SetTableUploadStatus(warehouseutils.ExecutingState, sf.Upload.ID, tableName, sf.DbHandle)
	timer := warehouseutils.DestStat(stats.TimerType, "single_table_upload_time", sf.Warehouse.Destination.ID)
	timer.Start()

	dbHandle, err := connect(sf.getConnectionCredentials(OptionalCredsT{schemaName: sf.Namespace}))
	if err != nil {
		logger.Errorf("SF: Error establishing connection for copying table:%s: %v\n", tableName, err)
		warehouseutils.SetTableUploadError(warehouseutils.ExportingDataFailedState, sf.Upload.ID, tableName, err, sf.DbHandle)
		return
	}
	defer dbHandle.Close()

	// sort columnnames
	keys := reflect.ValueOf(columnMap).MapKeys()
	strkeys := make([]string, len(keys))
	for i := 0; i < len(keys); i++ {
		strkeys[i] = keys[i].String()
	}
	sort.Strings(strkeys)
	var sortedColumnNames string
	for index, key := range strkeys {
		if index > 0 {
			sortedColumnNames += fmt.Sprintf(`, `)
		}
		sortedColumnNames += fmt.Sprintf(`%s`, key)
	}

	stagingTableName := fmt.Sprintf(`%s%s_%s`, stagingTablePrefix, tableName, strings.Replace(uuid.NewV4().String(), "-", "", -1))
	sqlStatement := fmt.Sprintf(`CREATE TEMPORARY TABLE %s LIKE %s`, stagingTableName, tableName)

	logger.Infof("SF: Creating temporary table for table:%s at %s\n", tableName, sqlStatement)
	_, err = dbHandle.Exec(sqlStatement)
	if err != nil {
		logger.Errorf("SF: Error creating temporary table for table:%s: %v\n", tableName, err)
		warehouseutils.SetTableUploadError(warehouseutils.ExportingDataFailedState, sf.Upload.ID, tableName, err, sf.DbHandle)
		return
	}

	csvObjectLocation, err := warehouseutils.GetLoadFileLocation(sf.DbHandle, sf.Warehouse.Source.ID, sf.Warehouse.Destination.ID, tableName, sf.Upload.StartLoadFileID, sf.Upload.EndLoadFileID)
	if err != nil {
		panic(err)
	}
	loadFolder := warehouseutils.GetS3LocationFolder(csvObjectLocation)

	sqlStatement = fmt.Sprintf(`COPY INTO %v(%v) FROM '%v' CREDENTIALS = (AWS_KEY_ID='%s' AWS_SECRET_KEY='%s') PATTERN = '.*\.csv\.gz'
		FILE_FORMAT = ( TYPE = csv FIELD_OPTIONALLY_ENCLOSED_BY = '"' ESCAPE_UNENCLOSED_FIELD = NONE )`, fmt.Sprintf(`%s.%s`, sf.Namespace, stagingTableName), sortedColumnNames, loadFolder, accessKeyID, accessKey)

	sanitisedSQLStmt, regexErr := misc.ReplaceMultiRegex(sqlStatement, map[string]string{
		"AWS_KEY_ID='[^']*'":     "AWS_KEY_ID='***'",
		"AWS_SECRET_KEY='[^']*'": "AWS_SECRET_KEY='***'",
	})
	if regexErr == nil {
		logger.Infof("SF: Running COPY command for table:%s at %s\n", tableName, sanitisedSQLStmt)
	}

	_, err = dbHandle.Exec(sqlStatement)
	if err != nil {
		logger.Errorf("SF: Error running COPY command: %v\n", err)
		warehouseutils.SetTableUploadError(warehouseutils.ExportingDataFailedState, sf.Upload.ID, tableName, err, sf.DbHandle)
		return
	}

	primaryKey := "ID"
	if column, ok := primaryKeyMap[tableName]; ok {
		primaryKey = column
	}

	var columnNames, stagingColumnNames, columnsWithValues string
	for idx, str := range strkeys {
		columnNames += fmt.Sprintf(`%s`, str)
		stagingColumnNames += fmt.Sprintf(`staging.%s`, str)
		columnsWithValues += fmt.Sprintf(`original.%[1]s = staging.%[1]s`, str)
		if idx != len(strkeys)-1 {
			columnNames += fmt.Sprintf(`,`)
			stagingColumnNames += fmt.Sprintf(`,`)
			columnsWithValues += fmt.Sprintf(`,`)
		}
	}

	sqlStatement = fmt.Sprintf(`MERGE INTO %[1]s AS original
									USING (
										SELECT * FROM (
											SELECT *, row_number() OVER (PARTITION BY %[3]s ORDER BY RECEIVED_AT ASC) AS _rudder_staging_row_number FROM %[2]s
										) AS q WHERE _rudder_staging_row_number = 1
									) AS staging
									ON original.%[3]s = staging.%[3]s
									WHEN MATCHED THEN
									UPDATE SET %[6]s
									WHEN NOT MATCHED THEN
									INSERT (%[4]s) VALUES (%[5]s)`, tableName, stagingTableName, primaryKey, columnNames, stagingColumnNames, columnsWithValues)
	logger.Infof("SF: Dedup records for table:%s using staging table: %s\n", tableName, sqlStatement)
	_, err = dbHandle.Exec(sqlStatement)
	if err != nil {
		logger.Errorf("SF: Error running MERGE for dedup: %v\n", err)
		warehouseutils.SetTableUploadError(warehouseutils.ExportingDataFailedState, sf.Upload.ID, tableName, err, sf.DbHandle)
		return
	}

	timer.End()
	warehouseutils.SetTableUploadStatus(warehouseutils.ExportedDataState, sf.Upload.ID, tableName, sf.DbHandle)
	logger.Infof("SF: Complete load for table:%s\n", tableName)
	return
}

func (sf *HandleT) load() (errList []error) {
	var accessKeyID, accessKey string
	config := sf.Warehouse.Destination.Config.(map[string]interface{})
	if config["accessKeyID"] != nil {
		accessKeyID = config["accessKeyID"].(string)
	}
	if config["accessKey"] != nil {
		accessKey = config["accessKey"].(string)
	}

	logger.Infof("SF: Starting load for all %v tables\n", len(sf.Upload.Schema))
	var wg sync.WaitGroup
	wg.Add(len(sf.Upload.Schema))
	loadChan := make(chan struct{}, maxParallelLoads)
	for tableName, columnMap := range sf.Upload.Schema {
		tName := tableName
		cMap := columnMap
		loadChan <- struct{}{}
		rruntime.Go(func() {
			loadError := sf.loadTable(tName, cMap, accessKeyID, accessKey)
			if loadError != nil {
				errList = append(errList, loadError)
			}
			wg.Done()
			<-loadChan
		})
	}
	wg.Wait()
	logger.Infof("SF: Completed load for all tables\n")
	return
}

type SnowflakeCredentialsT struct {
	account    string
	whName     string
	dbName     string
	username   string
	password   string
	schemaName string
}

func connect(cred SnowflakeCredentialsT) (*sql.DB, error) {
	url := fmt.Sprintf("%s:%s@%s/%s?warehouse=%s",
		cred.username,
		cred.password,
		cred.account,
		cred.dbName,
		cred.whName)

	if cred.schemaName != "" {
		url += fmt.Sprintf("&schema=%s", cred.schemaName)
	}

	var err error
	var db *sql.DB
	if db, err = sql.Open("snowflake", url); err != nil {
		return nil, fmt.Errorf("SF: snowflake connect error : (%v)", err)
	}

	alterStatement := fmt.Sprintf(`ALTER SESSION SET ABORT_DETACHED_QUERY=TRUE`)
	logger.Infof("SF: Altering session with abort_detached_query for snowflake: %v", alterStatement)
	_, err = db.Exec(alterStatement)
	if err != nil {
		return nil, fmt.Errorf("SF: snowflake alter session error : (%v)", err)
	}
	return db, nil
}

func loadConfig() {
	warehouseUploadsTable = config.GetString("Warehouse.uploadsTable", "wh_uploads")
	stagingTablePrefix = "rudder_staging_"
	maxParallelLoads = config.GetInt("Warehouse.snowflake.maxParallelLoads", 1)
}

func init() {
	config.Initialize()
	loadConfig()
}

func (sf *HandleT) MigrateSchema() (err error) {
	timer := warehouseutils.DestStat(stats.TimerType, "migrate_schema_time", sf.Warehouse.Destination.ID)
	timer.Start()
	warehouseutils.SetUploadStatus(sf.Upload, warehouseutils.UpdatingSchemaState, sf.DbHandle)
	logger.Infof("SF: Updating schema for snowflake schemaname: %s", sf.Namespace)
	updatedSchema, err := sf.updateSchema()
	if err != nil {
		warehouseutils.SetUploadError(sf.Upload, err, warehouseutils.UpdatingSchemaFailedState, sf.DbHandle)
		return
	}
	err = warehouseutils.SetUploadStatus(sf.Upload, warehouseutils.UpdatedSchemaState, sf.DbHandle)
	if err != nil {
		panic(err)
	}
	err = warehouseutils.UpdateCurrentSchema(sf.Namespace, sf.Warehouse, sf.Upload.ID, sf.CurrentSchema, updatedSchema, sf.DbHandle)
	timer.End()
	if err != nil {
		warehouseutils.SetUploadError(sf.Upload, err, warehouseutils.UpdatingSchemaFailedState, sf.DbHandle)
		return
	}
	return
}

func (sf *HandleT) Export() (err error) {
	logger.Infof("SF: Starting export to snowflake for source:%s and wh_upload:%v", sf.Warehouse.Source.ID, sf.Upload.ID)
	err = warehouseutils.SetUploadStatus(sf.Upload, warehouseutils.ExportingDataState, sf.DbHandle)
	if err != nil {
		panic(err)
	}
	timer := warehouseutils.DestStat(stats.TimerType, "upload_time", sf.Warehouse.Destination.ID)
	timer.Start()
	errList := sf.load()
	timer.End()
	if len(errList) > 0 {
		errStr := ""
		for idx, err := range errList {
			errStr += err.Error()
			if idx < len(errList)-1 {
				errStr += ", "
			}
		}
		warehouseutils.SetUploadError(sf.Upload, errors.New(errStr), warehouseutils.ExportingDataFailedState, sf.DbHandle)
		return errors.New(errStr)
	}
	err = warehouseutils.SetUploadStatus(sf.Upload, warehouseutils.ExportedDataState, sf.DbHandle)
	if err != nil {
		panic(err)
	}
	return
}

func (sf *HandleT) CrashRecover(config warehouseutils.ConfigT) (err error) {
	return
}

type OptionalCredsT struct {
	schemaName string
}

func (sf *HandleT) getConnectionCredentials(opts OptionalCredsT) SnowflakeCredentialsT {
	return SnowflakeCredentialsT{
		account:    sf.Warehouse.Destination.Config.(map[string]interface{})["account"].(string),
		whName:     sf.Warehouse.Destination.Config.(map[string]interface{})["warehouse"].(string),
		dbName:     sf.Warehouse.Destination.Config.(map[string]interface{})["database"].(string),
		username:   sf.Warehouse.Destination.Config.(map[string]interface{})["user"].(string),
		password:   sf.Warehouse.Destination.Config.(map[string]interface{})["password"].(string),
		schemaName: opts.schemaName,
	}
}

func (sf *HandleT) Process(config warehouseutils.ConfigT) (err error) {
	sf.DbHandle = config.DbHandle
	sf.Warehouse = config.Warehouse
	sf.Upload = config.Upload

	currSchema, err := warehouseutils.GetCurrentSchema(sf.DbHandle, sf.Warehouse)
	if err != nil {
		panic(err)
	}
	sf.CurrentSchema = currSchema.Schema
	sf.Namespace = strings.ToUpper(currSchema.Namespace)
	if sf.Namespace == "" {
		logger.Infof("SF: Namespace not found in currentschema for SF:%s, setting from upload: %s", sf.Warehouse.Destination.ID, sf.Upload.Namespace)
		sf.Namespace = strings.ToUpper(sf.Upload.Namespace)
	}

	sf.Db, err = connect(sf.getConnectionCredentials(OptionalCredsT{}))
	if err != nil {
		warehouseutils.SetUploadError(sf.Upload, err, warehouseutils.UpdatingSchemaFailedState, sf.DbHandle)
		return err
	}

	if config.Stage == "ExportData" {
		err = sf.Export()
	} else {
		err = sf.MigrateSchema()
		if err == nil {
			err = sf.Export()
		}
	}
	sf.Db.Close()
	return
}
