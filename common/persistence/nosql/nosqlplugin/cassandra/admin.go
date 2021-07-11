// Copyright (c) 2021 Uber Technologies, Inc.
// Portions of the Software are attributed to Copyright (c) 2020 Temporal Technologies Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cassandra

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/uber/cadence/common/config"
	"github.com/uber/cadence/common/persistence/nosql/nosqlplugin"
	"github.com/uber/cadence/common/persistence/nosql/nosqlplugin/cassandra/gocql"
	"github.com/uber/cadence/tools/cassandra"
	"github.com/uber/cadence/tools/common/schema"
)

const (
	testSchemaDir = "schema/cassandra/"
)

var _ nosqlplugin.AdminDB = (*cdb)(nil)

func (db *cdb) QueryOneRow(queryTemplate string, args ...interface{}) (row map[string]interface{}, err error) {
	query := db.session.Query(queryTemplate, args...)
	result := make(map[string]interface{})
	err = query.MapScan(result)
	return result, err
}

func (db *cdb) SetupTestDatabase() error {
	err := createCassandraKeyspace(db.session, db.cfg.Keyspace, 1, true)
	if err != nil {
		return err
	}
	cadencePackageDir, err := getCadencePackageDir()
	if err != nil {
		log.Fatal(err)
	}
	schemaDir := cadencePackageDir + testSchemaDir

	err = db.loadSchema([]string{"schema.cql"}, schemaDir)
	if err != nil {
		return err
	}
	err = db.loadVisibilitySchema([]string{"schema.cql"}, schemaDir)
	if err != nil {
		return err
	}
	return nil
}

// loadSchema from PersistenceTestCluster interface
func (db *cdb) loadSchema(fileNames []string, schemaDir string) error {
	workflowSchemaDir := schemaDir + "/cadence"
	err := loadCassandraSchema(workflowSchemaDir, fileNames, db.cfg.Hosts, db.cfg.Port, db.cfg.Keyspace, true, nil, db.cfg.ProtoVersion)
	if err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
		// TODO: should we remove the second condition?
		return err
	}
	return nil
}

// loadVisibilitySchema from PersistenceTestCluster interface
func (db *cdb) loadVisibilitySchema(fileNames []string, schemaDir string) error {
	workflowSchemaDir := schemaDir + "visibility"
	err := loadCassandraSchema(workflowSchemaDir, fileNames, db.cfg.Hosts, db.cfg.Port, db.cfg.Keyspace, false, nil, db.cfg.ProtoVersion)
	if err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
		// TODO: should we remove the second condition?
		return err
	}
	return nil
}

func getCadencePackageDir() (string, error) {
	cadencePackageDir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	cadenceIndex := strings.LastIndex(cadencePackageDir, "/cadence/")
	cadencePackageDir = cadencePackageDir[:cadenceIndex+len("/cadence/")]
	return cadencePackageDir, err
}

func (db *cdb) TeardownTestDatabase() error {
	err := dropCassandraKeyspace(db.session, db.cfg.Keyspace)
	if err != nil {
		return err
	}
	db.session.Close()
	return nil
}

// createCassandraKeyspace creates the keyspace using this session for given replica count
func createCassandraKeyspace(s gocql.Session, keyspace string, replicas int, overwrite bool) (err error) {
	// if overwrite flag is set, drop the keyspace and create a new one
	if overwrite {
		err = dropCassandraKeyspace(s, keyspace)
		if err != nil {
			log.Error(`drop keyspace error`, err)
			return
		}
	}
	err = s.Query(fmt.Sprintf(`CREATE KEYSPACE IF NOT EXISTS %s WITH replication = {
		'class' : 'SimpleStrategy', 'replication_factor' : %d}`, keyspace, replicas)).Exec()
	if err != nil {
		log.Error(`create keyspace error`, err)
		return
	}
	log.WithField(`keyspace`, keyspace).Debug(`created namespace`)

	return
}

// dropCassandraKeyspace drops the given keyspace, if it exists
func dropCassandraKeyspace(s gocql.Session, keyspace string) (err error) {
	err = s.Query(fmt.Sprintf("DROP KEYSPACE IF EXISTS %s", keyspace)).Exec()
	if err != nil {
		log.Error(`drop keyspace error`, err)
		return
	}
	log.WithField(`keyspace`, keyspace).Info(`dropped namespace`)
	return
}

// loadCassandraSchema loads the schema from the given .cql files on this keyspace
func loadCassandraSchema(
	dir string,
	fileNames []string,
	hosts string,
	port int,
	keyspace string,
	override bool,
	tls *config.TLS,
	protoVersion int,
) (err error) {

	tmpFile, err := ioutil.TempFile("", "_cadence_")
	if err != nil {
		return fmt.Errorf("error creating tmp file:%v", err.Error())
	}
	defer os.Remove(tmpFile.Name())

	for _, file := range fileNames {
		// Flagged for potential file inclusion via variable. No user supplied input is included here - this just reads
		// schema files.
		// #nosec
		content, err := ioutil.ReadFile(dir + "/" + file)
		if err != nil {
			return fmt.Errorf("error reading contents of file %v:%v", file, err.Error())
		}
		_, err = tmpFile.WriteString(string(content) + "\n")
		if err != nil {
			return fmt.Errorf("error writing string to file, err: %v", err.Error())
		}
	}

	tmpFile.Close()

	config := &cassandra.SetupSchemaConfig{
		CQLClientConfig: cassandra.CQLClientConfig{
			Hosts:        hosts,
			Port:         port,
			Keyspace:     keyspace,
			TLS:          tls,
			ProtoVersion: protoVersion,
		},
		SetupConfig: schema.SetupConfig{
			SchemaFilePath:    tmpFile.Name(),
			Overwrite:         override,
			DisableVersioning: true,
		},
	}

	err = cassandra.SetupSchema(config)
	if err != nil {
		err = fmt.Errorf("error loading schema:%v", err.Error())
	}
	return
}
