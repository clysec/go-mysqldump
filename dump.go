package mysqldump

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/template"
	"time"
)

/*
Data struct to configure dump behavior

	Out:              Stream to write to
	Connection:       Database connection to dump
	IgnoreTables:     Mark sensitive tables to ignore
	MaxAllowedPacket: Sets the largest packet size to use in backups
	LockTables:       Lock all tables for the duration of the dump
*/
type Data struct {
	Out              io.Writer
	Connection       *sql.DB
	IgnoreTables     []string
	MaxAllowedPacket int
	LockTables       bool

	tx         *sql.Tx
	headerTmpl *template.Template
	viewTmpl   *template.Template
	tableTmpl  *template.Template
	footerTmpl *template.Template
	err        error
}

type table struct {
	Name   string
	Err    error
	isView bool

	cols   []string
	data   *Data
	rows   *sql.Rows
	values []interface{}
}

type metaData struct {
	DumpVersion   string
	ServerVersion string
	CompleteTime  string
}

const (
	// Version of this plugin for easy reference
	Version = "0.7.0"

	defaultMaxAllowedPacket = 4194304
)

// takes a *metaData
const headerTmpl = `-- Go SQL Dump {{ .DumpVersion }}
--
-- ------------------------------------------------------
-- Server version	{{ .ServerVersion }}

/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
/*!40101 SET @OLD_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS */;
/*!40101 SET @OLD_COLLATION_CONNECTION=@@COLLATION_CONNECTION */;
 SET NAMES utf8mb4 ;
/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */;
/*!40103 SET TIME_ZONE='+00:00' */;
/*!40014 SET @OLD_UNIQUE_CHECKS=@@UNIQUE_CHECKS, UNIQUE_CHECKS=0 */;
/*!40014 SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0 */;
/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;
/*!40111 SET @OLD_SQL_NOTES=@@SQL_NOTES, SQL_NOTES=0 */;
`

// takes a *metaData
const footerTmpl = `/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

-- Dump completed on {{ .CompleteTime }}
`

// Takes a *table
const tableTmpl = `
--
-- Table structure for table {{ .NameEsc }}
--

DROP TABLE IF EXISTS {{ .NameEsc }};
/*!40101 SET @saved_cs_client     = @@character_set_client */;
 SET character_set_client = utf8mb4 ;
{{ .CreateSQL }};
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table {{ .NameEsc }}
--

LOCK TABLES {{ .NameEsc }} WRITE;
/*!40000 ALTER TABLE {{ .NameEsc }} DISABLE KEYS */;
{{ range $value := .Stream }}
{{- $value }}
{{ end -}}
/*!40000 ALTER TABLE {{ .NameEsc }} ENABLE KEYS */;
UNLOCK TABLES;
`
const viewTmpl = `
--
-- View structure for view {{ .NameEsc }}
--

DROP VIEW IF EXISTS {{ .NameEsc }};
/*!40101 SET @saved_cs_client     = @@character_set_client */;
 SET character_set_client = utf8mb4 ;
{{ .CreateSQL }};
/*!40101 SET character_set_client = @saved_cs_client */;
`

const nullType = "NULL"

// Dump data using struct
func (data *Data) DumpDatabase(database string) error {
	meta := metaData{
		DumpVersion: Version,
	}

	if data.MaxAllowedPacket == 0 {
		data.MaxAllowedPacket = defaultMaxAllowedPacket
	}

	if err := data.getTemplates(); err != nil {
		return err
	}

	// Start the read only transaction and defer the rollback until the end
	// This way the database will have the exact state it did at the beginning of
	// the backup and nothing can be accidentally committed
	if err := data.begin(); err != nil {
		return err
	}
	defer data.rollback()

	if err := data.useDatabase(database); err != nil {
		return err
	}

	if err := meta.updateServerVersion(data); err != nil {
		return err
	}

	if err := data.headerTmpl.Execute(data.Out, meta); err != nil {
		return err
	}

	tables, err := data.getTables()
	if err != nil {
		return err
	}

	// Lock all tables before dumping if present
	if data.LockTables && len(tables) > 0 {
		var b bytes.Buffer
		b.WriteString("LOCK TABLES ")
		for index, table := range tables {
			if index != 0 {
				b.WriteString(",")
			}
			b.WriteString("`" + table.Name + "` READ /*!32311 LOCAL */")
		}

		if _, err := data.Connection.Exec(b.String()); err != nil {
			return err
		}

		defer data.Connection.Exec("UNLOCK TABLES")
	}

	for _, table := range tables {
		if err := data.dumpTable(table); err != nil {
			return err
		}
	}

	if data.err != nil {
		return data.err
	}

	meta.CompleteTime = time.Now().String()
	return data.footerTmpl.Execute(data.Out, meta)
}

// Dump data using struct
func (data *Data) Dump() error {
	meta := metaData{
		DumpVersion: Version,
	}

	if data.MaxAllowedPacket == 0 {
		data.MaxAllowedPacket = defaultMaxAllowedPacket
	}

	if err := data.getTemplates(); err != nil {
		return err
	}

	// Start the read only transaction and defer the rollback until the end
	// This way the database will have the exact state it did at the beginning of
	// the backup and nothing can be accidentally committed
	if err := data.begin(); err != nil {
		return err
	}
	defer data.rollback()

	if err := meta.updateServerVersion(data); err != nil {
		return err
	}

	if err := data.headerTmpl.Execute(data.Out, meta); err != nil {
		return err
	}

	tables, err := data.getTables()
	if err != nil {
		return err
	}

	// Lock all tables before dumping if present
	if data.LockTables && len(tables) > 0 {
		var b bytes.Buffer
		b.WriteString("LOCK TABLES ")
		for index, table := range tables {
			if index != 0 {
				b.WriteString(",")
			}
			b.WriteString("`" + table.Name + "` READ /*!32311 LOCAL */")
		}

		if _, err := data.Connection.Exec(b.String()); err != nil {
			return err
		}

		defer data.Connection.Exec("UNLOCK TABLES")
	}

	for _, table := range tables {
		if err := data.dumpTable(table); err != nil {
			return err
		}
	}

	if data.err != nil {
		return data.err
	}

	meta.CompleteTime = time.Now().String()
	return data.footerTmpl.Execute(data.Out, meta)
}

// MARK: - Private methods

// begin starts a read only transaction that will be whatever the database was
// when it was called
func (data *Data) begin() (err error) {
	data.tx, err = data.Connection.BeginTx(context.Background(), &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	return
}

// Choose a database to dump
func (data *Data) useDatabase(database string) error {
	if data.tx == nil {
		return errors.New("transaction not started")
	}
	if _, err := data.tx.Exec("USE " + database); err != nil {
		return err
	}
	return nil
}

// rollback cancels the transaction
func (data *Data) rollback() error {
	return data.tx.Rollback()
}

// MARK: writer methods

func (data *Data) dumpTable(table *table) error {
	if data.err != nil {
		return data.err
	}
	return data.writeTable(table)
}

func (data *Data) writeTable(table *table) error {
	if table.isView {
		if err := data.viewTmpl.Execute(data.Out, table); err != nil {
			return err
		}
	} else {
		if err := data.tableTmpl.Execute(data.Out, table); err != nil {
			return err
		}
	}
	return table.Err
}

// MARK: get methods

// getTemplates initializes the templates on data from the constants in this file
func (data *Data) getTemplates() (err error) {
	data.headerTmpl, err = template.New("mysqldumpHeader").Parse(headerTmpl)
	if err != nil {
		return
	}

	data.tableTmpl, err = template.New("mysqldumpTable").Parse(tableTmpl)
	if err != nil {
		return
	}

	data.viewTmpl, err = template.New("mysqldumpView").Parse(viewTmpl)
	if err != nil {
		return
	}

	data.footerTmpl, err = template.New("mysqldumpTable").Parse(footerTmpl)
	if err != nil {
		return
	}
	return
}

func (data *Data) getTables() ([]*table, error) {
	tables := make([]*table, 0)

	rows, err := data.tx.Query("SHOW FULL TABLES")
	if err != nil {
		return tables, err
	}
	defer rows.Close()

	for rows.Next() {
		var tableName sql.NullString
		var tableType sql.NullString
		if err := rows.Scan(&tableName, &tableType); err != nil {
			return tables, err
		}
		if tableName.Valid && !data.isIgnoredTable(tableName.String) {
			table := data.createTable(tableName.String, tableType.String == "VIEW")
			tables = append(tables, table)
		}
	}
	return tables, rows.Err()
}

func (data *Data) isIgnoredTable(name string) bool {
	for _, item := range data.IgnoreTables {
		if item == name {
			return true
		}
	}
	return false
}

func (meta *metaData) updateServerVersion(data *Data) (err error) {
	var serverVersion sql.NullString
	err = data.tx.QueryRow("SELECT version()").Scan(&serverVersion)
	meta.ServerVersion = serverVersion.String
	return
}

// MARK: create methods

func (data *Data) createTable(name string, isView bool) *table {
	return &table{
		Name:   name,
		isView: isView,
		data:   data,
	}
}

func (table *table) NameEsc() string {
	return "`" + table.Name + "`"
}

func (table *table) CreateSQL() (string, error) {
	rows, err := table.data.tx.Query("SHOW CREATE TABLE " + table.NameEsc())
	if err != nil {
		return "", err
	}
	defer rows.Close()

	// get the column names from the query
	columnNames, err := rows.Columns()
	if err != nil {
		return "", err
	}
	columnCount := len(columnNames)

	info := make([]sql.NullString, columnCount)
	scans := make([]interface{}, columnCount)
	for i := range info {
		scans[i] = &info[i]
	}

	if rows.Next() {
		if err := rows.Scan(scans...); err != nil {
			return "", err
		}
	}

	if len(info) < 2 {
		return "", errors.New("database column information is malformed")
	}

	if info[0].String != table.Name {
		return "", errors.New("returned table is not the same as requested table")
	}

	table.isView = strings.Contains(info[1].String, "VIEW")

	return info[1].String, nil
}

func (table *table) initColumnData() error {
	colInfo, err := table.data.tx.Query("SHOW COLUMNS FROM " + table.NameEsc())
	if err != nil {
		return err
	}
	defer colInfo.Close()

	cols, err := colInfo.Columns()
	if err != nil {
		return err
	}

	fieldIndex, extraIndex := -1, -1
	for i, col := range cols {
		switch col {
		case "Field", "field":
			fieldIndex = i
		case "Extra", "extra":
			extraIndex = i
		}
		if fieldIndex >= 0 && extraIndex >= 0 {
			break
		}
	}
	if fieldIndex < 0 || extraIndex < 0 {
		return errors.New("database column information is malformed")
	}

	info := make([]sql.NullString, len(cols))
	scans := make([]interface{}, len(cols))
	for i := range info {
		scans[i] = &info[i]
	}

	var result []string
	for colInfo.Next() {
		// Read into the pointers to the info marker
		if err := colInfo.Scan(scans...); err != nil {
			return err
		}

		// Ignore the virtual columns
		if !info[extraIndex].Valid || !strings.Contains(info[extraIndex].String, "VIRTUAL") {
			result = append(result, info[fieldIndex].String)
		}
	}
	table.cols = result
	return nil
}

func (table *table) columnsList() string {
	return "`" + strings.Join(table.cols, "`, `") + "`"
}

func (table *table) Init() error {
	if len(table.values) != 0 {
		return errors.New("can't init twice")
	}

	if err := table.initColumnData(); err != nil {
		return err
	}

	if len(table.cols) == 0 {
		// No data to dump since this is a virtual table
		return nil
	}

	var err error
	table.rows, err = table.data.tx.Query("SELECT " + table.columnsList() + " FROM " + table.NameEsc())
	if err != nil {
		return err
	}

	tt, err := table.rows.ColumnTypes()
	if err != nil {
		return err
	}

	table.values = make([]interface{}, len(tt))
	for i, tp := range tt {
		table.values[i] = reflect.New(reflectColumnType(tp)).Interface()
	}
	return nil
}

func reflectColumnType(tp *sql.ColumnType) reflect.Type {
	// reflect for ScanType
	switch tp.ScanType().Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflect.TypeOf(sql.NullInt64{})
	case reflect.Float32, reflect.Float64:
		return reflect.TypeOf(sql.NullFloat64{})
	case reflect.String:
		return reflect.TypeOf(sql.NullString{})
	}

	// determine by name
	switch tp.DatabaseTypeName() {
	case "BLOB", "BINARY":
		return reflect.TypeOf(sql.RawBytes{})
	case "VARCHAR", "TEXT", "DECIMAL", "JSON":
		return reflect.TypeOf(sql.NullString{})
	case "BIGINT", "TINYINT", "INT":
		return reflect.TypeOf(sql.NullInt64{})
	case "DOUBLE":
		return reflect.TypeOf(sql.NullFloat64{})
	}

	// unknown datatype
	return tp.ScanType()
}

func (table *table) Next() bool {
	if table.rows == nil {
		if err := table.Init(); err != nil {
			table.Err = err
			return false
		}
	}
	// Fallthrough
	if table.rows.Next() {
		if err := table.rows.Scan(table.values...); err != nil {
			table.Err = err
			return false
		} else if err := table.rows.Err(); err != nil {
			table.Err = err
			return false
		}
	} else {
		table.rows.Close()
		table.rows = nil
		return false
	}
	return true
}

func (table *table) RowValues() string {
	return table.RowBuffer().String()
}

func (table *table) RowBuffer() *bytes.Buffer {
	var b bytes.Buffer
	b.WriteString("(")

	for key, value := range table.values {
		if key != 0 {
			b.WriteString(",")
		}
		switch s := value.(type) {
		case nil:
			b.WriteString(nullType)
		case *sql.NullString:
			if s.Valid {
				fmt.Fprintf(&b, "'%s'", sanitize(s.String))
			} else {
				b.WriteString(nullType)
			}
		case *sql.NullInt64:
			if s.Valid {
				fmt.Fprintf(&b, "%d", s.Int64)
			} else {
				b.WriteString(nullType)
			}
		case *sql.NullFloat64:
			if s.Valid {
				fmt.Fprintf(&b, "%f", s.Float64)
			} else {
				b.WriteString(nullType)
			}
		case *sql.RawBytes:
			if len(*s) == 0 {
				b.WriteString(nullType)
			} else {
				fmt.Fprintf(&b, "_binary '%s'", sanitize(string(*s)))
			}
		default:
			fmt.Fprintf(&b, "'%s'", value)
		}
	}
	b.WriteString(")")

	return &b
}

func (table *table) Stream() <-chan string {
	valueOut := make(chan string, 1)
	go func() {
		defer close(valueOut)
		var insert bytes.Buffer

		for table.Next() {
			b := table.RowBuffer()
			// Truncate our insert if it won't fit
			if insert.Len() != 0 && insert.Len()+b.Len() > table.data.MaxAllowedPacket-1 {
				insert.WriteString(";")
				valueOut <- insert.String()
				insert.Reset()
			}

			if insert.Len() == 0 {
				fmt.Fprint(&insert, "INSERT INTO ", table.NameEsc(), " (", table.columnsList(), ") VALUES ")
			} else {
				insert.WriteString(",")
			}
			b.WriteTo(&insert)
		}
		if insert.Len() != 0 {
			insert.WriteString(";")
			valueOut <- insert.String()
		}
	}()
	return valueOut
}
