package copydb

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/ghostferry"
	"github.com/sirupsen/logrus"
)

type CopydbFerry struct {
	Ferry         *ghostferry.Ferry
	controlServer *ghostferry.ControlServer
	config        *Config
}

func NewFerry(config *Config) *CopydbFerry {
	ferry := &ghostferry.Ferry{
		Config: config.Config,
	}

	controlServer := &ghostferry.ControlServer{
		F:       ferry,
		Addr:    config.ServerBindAddr,
		Basedir: config.WebBasedir,
	}

	return &CopydbFerry{
		Ferry:         ferry,
		controlServer: controlServer,
		config:        config,
	}
}

func (this *CopydbFerry) Initialize() error {
	if this.config.RunFerryFromReplica {
		err := this.initializeWaitUntilReplicaIsCaughtUpToMasterConnection()
		if err != nil {
			return err
		}
	}

	err := this.Ferry.Initialize()
	if err != nil {
		return err
	}

	this.controlServer.Verifier = this.Ferry.Verifier

	return this.controlServer.Initialize()
}

func (this *CopydbFerry) Start() error {
	return this.Ferry.Start()
}

func (this *CopydbFerry) CreateDatabasesAndTables() error {
	// We need to create the same table/schemas on the target database
	// as the ones we are copying.
	logrus.Info("creating databases and tables on target")

	tableReferences := make(map[ghostferry.QualifiedTableName]ghostferry.TableForeignKeys)
	for tableName := range this.Ferry.Tables {
		t := strings.Split(tableName, ".")
		table := ghostferry.NewQualifiedTableName(t[0], t[1])

		referencedTables, err := ghostferry.GetForeignKeyTablesOfTable(this.Ferry.SourceDB, table)
		if err != nil {
			logrus.WithError(err).WithField("table", table).Error("cannot analyze database table foreign keys")
			return err
		}

		logrus.Debugf("found %d reference tables for %s", len(referencedTables), table)
		tableReferences[table] = referencedTables
	}

	// simple fix-point loop: make sure we create at least one table per
	// iteration and mark tables as able to create as soon as they no-longer
	// refer to other tables
	for len(tableReferences) > 0 {
		createdTable := false
		for table, referencedTables := range tableReferences {
			if len(referencedTables) > 0 {
				continue
			}
			logrus.Debugf("creating database table %s", table)

			err := this.createDatabaseIfExistsOnTarget(table.SchemaName)
			if err != nil {
				logrus.WithError(err).WithField("database", table.SchemaName).Error("cannot create database, this may leave the target database in an insane state")
				return err
			}

			err = this.createTableOnTarget(table.SchemaName, table.TableName)
			if err != nil {
				logrus.WithError(err).WithField("table", table).Error("cannot create table, this may leave the target database in an insane state")
				return err
			}

			// mark any table referring to the table as potential candidates
			// for being created now
			for otherTable, otherReferencedTables := range tableReferences {
				if _, found := otherReferencedTables[table]; found {
					delete(otherReferencedTables, table)
					if len(otherReferencedTables) == 0 {
						logrus.Debugf("creation of %s unblocked creation of %s", table, otherTable)
					}
				}

			}

			delete(tableReferences, table)
			createdTable = true
		}

		if !createdTable {
			return fmt.Errorf("failed creating tables: all %d remaining tables have foreign references", len(tableReferences))
		}
	}

	return nil
}

func (this *CopydbFerry) Run() {
	serverWG := &sync.WaitGroup{}
	serverWG.Add(1)
	go this.controlServer.Run(serverWG)

	copyWG := &sync.WaitGroup{}
	copyWG.Add(1)
	go func() {
		defer copyWG.Done()
		this.Ferry.Run()
	}()

	// If AutomaticCutover == false, it will pause below the following line
	this.Ferry.WaitUntilRowCopyIsComplete()

	// This waits until we're pretty close in the binlog before making the
	// source readonly. This is to avoid excessive downtime caused by the
	// binlog streamer catching up.
	this.Ferry.WaitUntilBinlogStreamerCatchesUp()

	// This is when the source database should be set as read only, whether it
	// is done in application level or the database level.
	// Must ensure that all transactions are flushed to the binlog before
	// proceeding.
	this.Ferry.FlushBinlogAndStopStreaming()

	// After waiting for the binlog streamer to stop, the source and the target
	// should be identical.
	copyWG.Wait()

	// This is where you cutover from using the source database to
	// using the target database.
	logrus.Info("ghostferry main operations has terminated but the control server remains online")
	logrus.Info("press CTRL+C or send an interrupt to stop the control server and end this process")

	// Work is done, the process will run the web server until killed.
	serverWG.Wait()
}

func (this *CopydbFerry) ShutdownControlServer() error {
	return this.controlServer.Shutdown()
}

func (this *CopydbFerry) initializeWaitUntilReplicaIsCaughtUpToMasterConnection() error {
	masterDB, err := this.config.SourceReplicationMaster.SqlDB(logrus.WithField("tag", "copydb"))
	if err != nil {
		return err
	}

	positionFetcher := ghostferry.ReplicatedMasterPositionViaCustomQuery{Query: this.config.ReplicatedMasterPositionQuery}

	var timeout time.Duration
	if this.config.WaitForReplicationTimeout == "" {
		timeout = time.Duration(0)
	} else {
		timeout, err = time.ParseDuration(this.config.WaitForReplicationTimeout)
		if err != nil {
			return err
		}
	}

	this.Ferry.WaitUntilReplicaIsCaughtUpToMaster = &ghostferry.WaitUntilReplicaIsCaughtUpToMaster{
		MasterDB:                        masterDB,
		Timeout:                         timeout,
		ReplicatedMasterPositionFetcher: positionFetcher,
	}
	return nil
}

func (this *CopydbFerry) createDatabaseIfExistsOnTarget(database string) error {
	if targetDbName, exists := this.Ferry.DatabaseRewrites[database]; exists {
		database = targetDbName
	}

	createDatabaseQuery := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", database)
	_, err := this.Ferry.TargetDB.Exec(createDatabaseQuery)
	return err
}

func (this *CopydbFerry) createTableOnTarget(database, table string) error {
	var tableNameAgain, createTableQuery string

	r := this.Ferry.SourceDB.QueryRow(fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", database, table))
	err := r.Scan(&tableNameAgain, &createTableQuery)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error":    err,
			"database": database,
			"table":    table,
		}).Error("unable to show table on source")
		return err
	}

	if targetDbName, exists := this.Ferry.DatabaseRewrites[database]; exists {
		database = targetDbName
	}

	if targetTableName, exists := this.Ferry.TableRewrites[tableNameAgain]; exists {
		tableNameAgain = targetTableName
	}

	createTableQueryReplaced := strings.Replace(
		createTableQuery,
		fmt.Sprintf("CREATE TABLE `%s`", table),
		fmt.Sprintf("CREATE TABLE `%s`.`%s`", database, tableNameAgain),
		1,
	)

	if createTableQueryReplaced == createTableQuery {
		return fmt.Errorf("no effect on replacing the create table <table> with create table <db>.<table> query on query: %s", createTableQuery)
	}

	_, err = this.Ferry.TargetDB.Exec(createTableQueryReplaced)
	return err
}
