package river

import (
	"fmt"
	"github.com/siddontang/go-mysql-elasticsearch/dump"
	"github.com/siddontang/go-mysql/mysql"
	"github.com/siddontang/go-mysql/schema"
	"github.com/siddontang/go/log"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"time"
)

type parseHandler struct {
	r *River

	name string
	pos  uint64
}

func (h *parseHandler) BinLog(name string, pos uint64) error {
	h.name = name
	h.pos = pos
	return nil
}

func (h *parseHandler) Data(db string, table string, values []string) error {
	rule, ok := h.r.rules[ruleKey(db, table)]
	if !ok {
		// no rule, skip this data
		log.Warnf("no rule for %s.%s", db, table)
		return nil
	}

	vs := make([]interface{}, len(values))

	for i, v := range values {
		if v == "NULL" {
			vs[i] = nil
		} else if v[0] != '\'' {
			if rule.TableInfo.Columns[i].Type == schema.TYPE_NUMBER {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					log.Errorf("paser row %v at %d error %v, skip", values, i, err)
					return dump.ErrSkip
				}
				vs[i] = n
			} else if rule.TableInfo.Columns[i].Type == schema.TYPE_FLOAT {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					log.Errorf("paser row %v at %d error %v, skip", values, i, err)
					return dump.ErrSkip
				}
				vs[i] = f
			} else {
				log.Errorf("paser row %v error, invalid type at %d, skip", values, i)
				return dump.ErrSkip
			}
		} else {
			vs[i] = v[1 : len(v)-1]
		}
	}

	if err := h.r.syncDocument(rule, syncInsertDoc, [][]interface{}{vs}, false); err != nil {
		log.Errorf("dump: sync %v  error %v", vs, err)
	}

	return nil
}

func (r *River) tryDump() error {
	if len(r.m.Name) > 0 && r.m.Position > 0 {
		// we will sync with binlog name and position
		log.Infof("skip dump, use last binlog replication pos (%s, %d)", r.m.Name, r.m.Position)
		return nil
	}

	if r.dumper == nil {
		log.Info("skip dump, no mysqldump")
		return nil
	}

	name := path.Join(r.c.DataDir, fmt.Sprintf("%s.sql", time.Now().String()))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(name)
	}()

	var db string
	dbs := map[string]struct{}{}
	tables := make([]string, 0, len(r.rules))
	for _, rule := range r.rules {
		db = rule.Schema
		dbs[rule.Schema] = struct{}{}
		tables = append(tables, rule.Table)
	}

	if len(dbs) == 1 {
		// one db, we can shrink using table
		r.dumper.AddTables(db, tables...)
	} else {
		// many dbs, can only assign databases to dump
		keys := make([]string, 0, len(dbs))
		for key, _ := range dbs {
			keys = append(keys, key)
		}

		r.dumper.AddDatabases(keys...)
	}

	r.dumper.SetErrOut(ioutil.Discard)

	t := time.Now()
	log.Info("try dump MySQL")
	if err = r.dumper.Dump(f); err != nil {
		return err
	}

	n := time.Now()
	log.Infof("dump MySQL OK, use %0.2f seconds, try parse", n.Sub(t).Seconds())

	f.Seek(0, 0)

	// do we need to delete the associated index in Elasticserach????
	if err = dump.Parse(f, r.parser); err != nil {
		return err
	}

	pos := mysql.Position{r.parser.name, uint32(r.parser.pos)}
	// set binlog information for sync
	r.ev <- pos
	r.waitPos(pos, 60)

	t = time.Now()

	log.Infof("parse dump MySQL data OK, use %0.2f seconds, start binlog replication at %v",
		t.Sub(n).Seconds(), pos)

	return nil
}
