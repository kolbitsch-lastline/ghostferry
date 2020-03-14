package ghostferry

import (
	sqlorig "database/sql"
	"fmt"
	sql "github.com/Shopify/ghostferry/sqlwrapper"
	"strings"

	"github.com/Masterminds/squirrel"
	"github.com/siddontang/go-mysql/schema"
	"github.com/sirupsen/logrus"
)

// both `sql.Tx` and `sql.DB` allow a SQL query to be `Prepare`d
type SqlPreparer interface {
	Prepare(string) (*sqlorig.Stmt, error)
}

type SqlDBWithFakeRollback struct {
	*sql.DB
}

func (d *SqlDBWithFakeRollback) Rollback() error {
	return nil
}

// sql.DB does not implement Rollback, but can use SqlDBWithFakeRollback
// to perform a noop.
type SqlPreparerAndRollbacker interface {
	SqlPreparer
	Rollback() error
}

type CursorConfig struct {
	DB        *sql.DB
	Throttler Throttler

	ColumnsToSelect []string
	BuildSelect     func([]string, *TableSchema, uint64, uint64) (squirrel.SelectBuilder, error)
	BatchSize       uint64
	ReadRetries     int
}

// returns a new PaginatedCursor with an embedded copy of itself
func (c *CursorConfig) NewPaginatedCursor(table *TableSchema, startPaginationKey, maxPaginationKey uint64) *PaginatedCursor {
	return &PaginatedCursor{
		CursorConfig:                *c,
		Table:                       table,
		MaxPaginationKey:            maxPaginationKey,
		RowLock:                     true,
		lastSuccessfulPaginationKey: startPaginationKey,
	}
}

// returns a new PaginatedCursor with an embedded copy of itself
func (c *CursorConfig) NewPaginatedCursorWithoutRowLock(table *TableSchema, startPaginationKey, maxPaginationKey uint64) *PaginatedCursor {
	cursor := c.NewPaginatedCursor(table, startPaginationKey, maxPaginationKey)
	cursor.RowLock = false
	return cursor
}

type PaginatedCursor struct {
	CursorConfig

	Table            *TableSchema
	MaxPaginationKey uint64
	RowLock          bool

	paginationKeyColumn         *schema.TableColumn
	lastSuccessfulPaginationKey uint64
	logger                      *logrus.Entry
}

func (c *PaginatedCursor) Each(f func(InsertRowBatch) error) error {
	c.logger = logrus.WithFields(logrus.Fields{
		"table": c.Table.String(),
		"tag":   "cursor",
	})
	c.paginationKeyColumn = c.Table.GetPaginationColumn()

	if len(c.ColumnsToSelect) == 0 {
		c.ColumnsToSelect = []string{"*"}
	}

	for c.lastSuccessfulPaginationKey < c.MaxPaginationKey {
		var tx SqlPreparerAndRollbacker
		var batch InsertRowBatch
		var paginationKeypos uint64

		err := WithRetries(c.ReadRetries, 0, c.logger, "fetch rows", func() (err error) {
			if c.Throttler != nil {
				WaitForThrottle(c.Throttler)
			}

			// Only need to use a transaction if RowLock == true. Otherwise
			// we'd be wasting two extra round trips per batch, doing
			// essentially a no-op.
			if c.RowLock {
				tx, err = c.DB.Begin()
				if err != nil {
					return err
				}
			} else {
				tx = &SqlDBWithFakeRollback{c.DB}
			}

			batch, paginationKeypos, err = c.Fetch(tx)
			if err == nil {
				return nil
			}

			tx.Rollback()
			return err
		})

		if err != nil {
			return err
		}

		if batch.Size() == 0 {
			tx.Rollback()
			c.logger.Debug("did not reach max primary key, but the table is complete as there are no more rows")
			break
		}

		if paginationKeypos <= c.lastSuccessfulPaginationKey {
			tx.Rollback()
			err = fmt.Errorf("new paginationKeypos %d <= lastSuccessfulPaginationKey %d", paginationKeypos, c.lastSuccessfulPaginationKey)
			c.logger.WithError(err).Errorf("last successful paginationKey position did not advance")
			return err
		}

		err = f(batch)
		if err != nil {
			tx.Rollback()
			c.logger.WithError(err).Error("failed to call each callback")
			return err
		}

		tx.Rollback()

		c.lastSuccessfulPaginationKey = paginationKeypos
	}

	return nil
}

func (c *PaginatedCursor) Fetch(db SqlPreparer) (batch InsertRowBatch, paginationKeypos uint64, err error) {
	var selectBuilder squirrel.SelectBuilder

	if c.BuildSelect != nil {
		selectBuilder, err = c.BuildSelect(c.ColumnsToSelect, c.Table, c.lastSuccessfulPaginationKey, c.BatchSize)
		if err != nil {
			c.logger.WithError(err).Error("failed to apply filter for select")
			return
		}
	} else {
		selectBuilder = DefaultBuildSelect(c.ColumnsToSelect, c.Table, c.lastSuccessfulPaginationKey, c.BatchSize)
	}

	if c.RowLock {
		selectBuilder = selectBuilder.Suffix("FOR UPDATE")
	}

	query, args, err := selectBuilder.ToSql()
	if err != nil {
		c.logger.WithError(err).Error("failed to build chunking sql")
		return
	}

	// With the inline verifier, the columns to be selected may be very large as
	// the query generated will be very large. The code here simply hides the
	// columns from the logger to not spam the logs.

	splitQuery := strings.Split(query, "FROM")
	loggedQuery := fmt.Sprintf("SELECT [omitted] FROM %s", splitQuery[1])

	logger := c.logger.WithFields(logrus.Fields{
		"sql":  loggedQuery,
		"args": args,
	})

	// This query must be a prepared query. If it is not, querying will use
	// MySQL's plain text interface, which will scan all values into []uint8
	// if we give it []interface{}.
	stmt, err := db.Prepare(query)
	if err != nil {
		logger.WithError(err).Error("failed to prepare query")
		return
	}

	defer stmt.Close()

	rows, err := stmt.Query(args...)
	if err != nil {
		logger.WithError(err).Error("failed to query database")
		return
	}

	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		logger.WithError(err).Error("failed to get columns")
		return
	}

	var paginationKeyIndex int = -1
	for idx, col := range columns {
		if col == c.paginationKeyColumn.Name {
			paginationKeyIndex = idx
			break
		}
	}

	if paginationKeyIndex < 0 {
		err = fmt.Errorf("paginationKey is not found during iteration with columns: %v", columns)
		logger.WithError(err).Error("failed to get paginationKey index")
		return
	}

	var rowData RowData
	var batchData []RowData

	for rows.Next() {
		rowData, err = ScanGenericRow(rows, len(columns))
		if err != nil {
			logger.WithError(err).Error("failed to scan row")
			return
		}

		batchData = append(batchData, rowData)
	}

	err = rows.Err()
	if err != nil {
		return
	}

	if len(batchData) > 0 {
		paginationKeypos, err = batchData[len(batchData)-1].GetUint64(paginationKeyIndex)
		if err != nil {
			logger.WithError(err).Error("failed to get uint64 paginationKey value")
			return
		}
	}

	batch = NewDataRowBatchWithPaginationKey(c.Table, batchData, paginationKeyIndex)
	logger.Debugf("found %d rows", batch.Size())

	return
}

// returns a new PaginatedCursor with an embedded copy of itself
func (c *CursorConfig) NewFullTableCursor(table *TableSchema) *FullTableCursor {
	return &FullTableCursor{
		DB:          c.DB,
		Table:       table,
		BatchSize:   c.BatchSize,
		ReadRetries: c.ReadRetries,
	}
}

type FullTableCursor struct {
	DB          *sql.DB
	Table       *TableSchema
	BatchSize   uint64
	ReadRetries int

	logger *logrus.Entry
}

func (c *FullTableCursor) Each(f func(RowBatch) error) error {
	c.logger = logrus.WithFields(logrus.Fields{
		"table": c.Table.String(),
		"tag":   "fullTableCursor",
	})

	// we do not support pagination, so we cannot resume copying of full-table
	// copies. We need to send out a way to re-initialize and prepare for a
	// full-table copy
	initBatch := NewTruncateTableBatch(c.Table)
	err := f(initBatch)
	if err != nil {
		c.logger.WithError(err).Error("failed to call init-each callback")
		return err
	}

	err = WithRetries(c.ReadRetries, 0, c.logger, "fetch rows", func() (err error) {
		tx, err := c.DB.Begin()
		if err != nil {
			return err
		}
		defer func() {
			_, err := tx.Query("UNLOCK TABLES")
			if err != nil {
				c.logger.WithError(err).Error("unlocking table failed")
			}
			tx.Rollback()
		}()

		// NOTE: We need to hold the row-lock on all rows for the entire
		// operation. Yes, crazy, but if we can't paginate, what are we supposed
		// to do? We really, *really*, *really* only use this for small tables
		_, err = tx.Query(fmt.Sprintf("LOCK TABLES %s WRITE", QuotedTableName(c.Table)))
		if err != nil {
			c.logger.WithError(err).Error("locking table failed")
			return err
		}

		rowOffset := 0
		for {
			c.logger.Debugf("fetching full-table batch at offset %d", rowOffset)
			batch, err := c.Fetch(tx, rowOffset)
			if err != nil {
				c.logger.WithError(err).Errorf("failed to invoke fetch at offset %d", rowOffset)
				return err
			}

			err = f(batch)
			if err != nil {
				c.logger.WithError(err).Error("failed to call each callback")
				return err
			}

			if batch.Size() < int(c.BatchSize) {
				c.logger.Debugf("there are no more rows to copy: last batch contained %d/%d rows", batch.Size(), c.BatchSize)
				return nil
			}

			rowOffset += batch.Size()
		}
	})

	return err
}

func (c *FullTableCursor) Fetch(db SqlPreparer, rowOffset int) (batch InsertRowBatch, err error) {
	// NOTE: The caller already locked the table for us
	selectBuilder := squirrel.Select("*").
		From(QuotedTableName(c.Table)).
		Limit(c.BatchSize).
		Offset(uint64(rowOffset))
	query, args, err := selectBuilder.ToSql()
	if err != nil {
		c.logger.WithError(err).Error("failed to build limit-offset sql")
		return
	}

	splitQuery := strings.Split(query, "FROM")
	loggedQuery := fmt.Sprintf("SELECT [omitted] FROM %s", splitQuery[1])

	logger := c.logger.WithFields(logrus.Fields{
		"sql":  loggedQuery,
		"args": args,
	})

	// This query must be a prepared query. If it is not, querying will use
	// MySQL's plain text interface, which will scan all values into []uint8
	// if we give it []interface{}.
	stmt, err := db.Prepare(query)
	if err != nil {
		logger.WithError(err).Error("failed to prepare query")
		return
	}

	defer stmt.Close()

	rows, err := stmt.Query(args...)
	if err != nil {
		logger.WithError(err).Error("failed to query database")
		return
	}

	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		logger.WithError(err).Error("failed to get columns")
		return
	}

	var rowData RowData
	var batchData []RowData

	for rows.Next() {
		rowData, err = ScanGenericRow(rows, len(columns))
		if err != nil {
			logger.WithError(err).Error("failed to scan row")
			return
		}

		batchData = append(batchData, rowData)
	}

	err = rows.Err()
	if err != nil {
		return
	}

	batch = NewDataRowBatch(c.Table, batchData)
	logger.Debugf("found %d rows", batch.Size())

	return
}

func ScanGenericRow(rows *sqlorig.Rows, columnCount int) (RowData, error) {
	values := make(RowData, columnCount)
	valuePtrs := make(RowData, columnCount)

	for i, _ := range values {
		valuePtrs[i] = &values[i]
	}

	err := rows.Scan(valuePtrs...)
	return values, err
}

func ScanByteRow(rows *sqlorig.Rows, columnCount int) ([][]byte, error) {
	values := make([][]byte, columnCount)
	valuePtrs := make(RowData, columnCount)

	for i, _ := range values {
		valuePtrs[i] = &values[i]
	}

	err := rows.Scan(valuePtrs...)
	return values, err
}

func DefaultBuildSelect(columns []string, table *TableSchema, lastPaginationKey, batchSize uint64) squirrel.SelectBuilder {
	quotedPaginationKey := quoteField(table.GetPaginationColumn().Name)

	return squirrel.Select(columns...).
		From(QuotedTableName(table)).
		Where(squirrel.Gt{quotedPaginationKey: lastPaginationKey}).
		Limit(batchSize).
		OrderBy(quotedPaginationKey)
}
