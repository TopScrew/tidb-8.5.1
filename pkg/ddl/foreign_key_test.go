// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/meta/model"
	pmodel "github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessiontxn"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/testkit/testfailpoint"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/stretchr/testify/require"
)

func testCreateForeignKey(t *testing.T, d ddl.ExecutorForTest, ctx sessionctx.Context, dbInfo *model.DBInfo, tblInfo *model.TableInfo, fkName string, keys []string, refTable string, refKeys []string, onDelete pmodel.ReferOptionType, onUpdate pmodel.ReferOptionType) *model.Job {
	FKName := pmodel.NewCIStr(fkName)
	Keys := make([]pmodel.CIStr, len(keys))
	for i, key := range keys {
		Keys[i] = pmodel.NewCIStr(key)
	}

	RefTable := pmodel.NewCIStr(refTable)
	RefKeys := make([]pmodel.CIStr, len(refKeys))
	for i, key := range refKeys {
		RefKeys[i] = pmodel.NewCIStr(key)
	}

	fkInfo := &model.FKInfo{
		Name:     FKName,
		RefTable: RefTable,
		RefCols:  RefKeys,
		Cols:     Keys,
		OnDelete: int(onDelete),
		OnUpdate: int(onUpdate),
		State:    model.StateNone,
	}

	job := &model.Job{
		Version:    model.GetJobVerInUse(),
		SchemaID:   dbInfo.ID,
		SchemaName: dbInfo.Name.L,
		TableID:    tblInfo.ID,
		TableName:  tblInfo.Name.L,
		Type:       model.ActionAddForeignKey,
		BinlogInfo: &model.HistoryInfo{},
	}
	err := sessiontxn.NewTxn(context.Background(), ctx)
	require.NoError(t, err)
	ctx.SetValue(sessionctx.QueryString, "skip")

	args := &model.AddForeignKeyArgs{FkInfo: fkInfo}
	err = d.DoDDLJobWrapper(ctx, ddl.NewJobWrapperWithArgs(job, args, true))
	require.NoError(t, err)
	return job
}

func testDropForeignKey(t *testing.T, ctx sessionctx.Context, d ddl.ExecutorForTest, dbInfo *model.DBInfo, tblInfo *model.TableInfo, foreignKeyName string) *model.Job {
	job := &model.Job{
		Version:    model.GetJobVerInUse(),
		SchemaID:   dbInfo.ID,
		SchemaName: dbInfo.Name.L,
		TableID:    tblInfo.ID,
		TableName:  tblInfo.Name.L,
		Type:       model.ActionDropForeignKey,
		BinlogInfo: &model.HistoryInfo{},
	}
	ctx.SetValue(sessionctx.QueryString, "skip")
	args := &model.DropForeignKeyArgs{FkName: pmodel.NewCIStr(foreignKeyName)}
	err := d.DoDDLJobWrapper(ctx, ddl.NewJobWrapperWithArgs(job, args, true))
	require.NoError(t, err)
	v := getSchemaVer(t, ctx)
	checkHistoryJobArgs(t, ctx, job.ID, &historyJobArgs{ver: v, tbl: tblInfo})
	return job
}

func getForeignKey(t table.Table, name string) *model.FKInfo {
	for _, fk := range t.Meta().ForeignKeys {
		// only public foreign key can be read.
		if fk.State != model.StatePublic {
			continue
		}
		if fk.Name.L == strings.ToLower(name) {
			return fk
		}
	}
	return nil
}

func TestForeignKey(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomainWithSchemaLease(t, testLease)

	dbInfo, err := testSchemaInfo(store, "test_foreign")
	require.NoError(t, err)
	de := dom.DDLExecutor().(ddl.ExecutorForTest)
	testCreateSchema(t, testkit.NewTestKit(t, store).Session(), de, dbInfo)
	tblInfo, err := testTableInfo(store, "t", 3)
	require.NoError(t, err)
	tblInfo.Indices = append(tblInfo.Indices, &model.IndexInfo{
		ID:    1,
		Name:  pmodel.NewCIStr("idx_fk"),
		Table: pmodel.NewCIStr("t"),
		Columns: []*model.IndexColumn{{
			Name:   pmodel.NewCIStr("c1"),
			Offset: 0,
			Length: types.UnspecifiedLength,
		}},
		State: model.StatePublic,
	})
	testCreateTable(t, testkit.NewTestKit(t, store).Session(), de, dbInfo, tblInfo)

	// fix data race
	var mu sync.Mutex
	checkOK := false
	var hookErr error
	testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/ddl/onJobUpdated", func(job *model.Job) {
		if job.State != model.JobStateDone {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var t table.Table
		t, err = testGetTableWithError(dom, dbInfo.ID, tblInfo.ID)
		if err != nil {
			hookErr = errors.Trace(err)
			return
		}
		fk := getForeignKey(t, "c1_fk")
		if fk == nil {
			hookErr = errors.New("foreign key not exists")
			return
		}
		checkOK = true
	})

	ctx := testkit.NewTestKit(t, store).Session()
	job := testCreateForeignKey(t, de, ctx, dbInfo, tblInfo, "c1_fk", []string{"c1"}, "t2", []string{"c1"}, pmodel.ReferOptionCascade, pmodel.ReferOptionSetNull)
	testCheckJobDone(t, store, job.ID, true)
	require.NoError(t, err)
	mu.Lock()
	hErr := hookErr
	ok := checkOK
	mu.Unlock()
	require.NoError(t, hErr)
	require.True(t, ok)
	v := getSchemaVer(t, ctx)
	checkHistoryJobArgs(t, ctx, job.ID, &historyJobArgs{ver: v, tbl: tblInfo})

	mu.Lock()
	checkOK = false
	mu.Unlock()
	// fix data race pr/#9491
	testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/ddl/onJobUpdated", func(job *model.Job) {
		if job.State != model.JobStateDone {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var t table.Table
		t, err = testGetTableWithError(dom, dbInfo.ID, tblInfo.ID)
		if err != nil {
			hookErr = errors.Trace(err)
			return
		}
		fk := getForeignKey(t, "c1_fk")
		if fk != nil {
			hookErr = errors.New("foreign key has not been dropped")
			return
		}
		checkOK = true
	})

	job = testDropForeignKey(t, ctx, de, dbInfo, tblInfo, "c1_fk")
	testCheckJobDone(t, store, job.ID, false)
	mu.Lock()
	hErr = hookErr
	ok = checkOK
	mu.Unlock()
	require.NoError(t, hErr)
	require.True(t, ok)
	testfailpoint.Disable(t, "github.com/pingcap/tidb/pkg/ddl/onJobUpdated")

	tk := testkit.NewTestKit(t, store)
	jobID := testDropTable(tk, t, dbInfo.Name.L, tblInfo.Name.L, dom)
	testCheckJobDone(t, store, jobID, false)

	require.NoError(t, err)
}

func TestTruncateOrDropTableWithForeignKeyReferred2(t *testing.T) {
	store := testkit.CreateMockStoreWithSchemaLease(t, testLease)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("set @@global.tidb_enable_foreign_key=1")
	tk.MustExec("set @@foreign_key_checks=1;")
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("set @@global.tidb_enable_foreign_key=1")
	tk2.MustExec("set @@foreign_key_checks=1;")
	tk2.MustExec("use test")

	tk.MustExec("create table t1 (id int key, a int);")

	var wg sync.WaitGroup
	var truncateErr, dropErr error
	testTruncate := true
	testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/ddl/onJobRunBefore", func(job *model.Job) {
		if job.SchemaState != model.StateNone {
			return
		}
		if job.Type != model.ActionCreateTable {
			return
		}
		wg.Add(1)
		if testTruncate {
			go func() {
				defer wg.Done()
				truncateErr = tk2.ExecToErr("truncate table t1")
			}()
		} else {
			go func() {
				defer wg.Done()
				dropErr = tk2.ExecToErr("drop table t1")
			}()
		}
		// make sure tk2's ddl job already put into ddl job queue.
		time.Sleep(time.Millisecond * 100)
	})

	tk.MustExec("create table t2 (a int, b int, foreign key fk(b) references t1(id));")
	wg.Wait()
	require.Error(t, truncateErr)
	require.Equal(t, "[ddl:1701]Cannot truncate a table referenced in a foreign key constraint (`test`.`t2` CONSTRAINT `fk`)", truncateErr.Error())

	tk.MustExec("drop table t2")
	testTruncate = false
	tk.MustExec("create table t2 (a int, b int, foreign key fk(b) references t1(id));")
	wg.Wait()
	require.Error(t, dropErr)
	require.Equal(t, "[ddl:1701]Cannot truncate a table referenced in a foreign key constraint (`test`.`t2` CONSTRAINT `fk`)", dropErr.Error())
}

func TestDropIndexNeededInForeignKey2(t *testing.T) {
	store := testkit.CreateMockStoreWithSchemaLease(t, testLease)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("set @@global.tidb_enable_foreign_key=1")
	tk.MustExec("set @@foreign_key_checks=1;")
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("set @@global.tidb_enable_foreign_key=1")
	tk2.MustExec("set @@foreign_key_checks=1;")
	tk2.MustExec("use test")
	tk.MustExec("create table t1 (id int key, b int)")
	tk.MustExec("create table t2 (a int, b int, index idx1 (b),index idx2 (b), foreign key (b) references t1(id));")

	var wg sync.WaitGroup
	var dropErr error
	testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/ddl/onJobRunBefore", func(job *model.Job) {
		if job.SchemaState != model.StatePublic || job.Type != model.ActionDropIndex {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			dropErr = tk2.ExecToErr("alter table t2 drop index idx2")
		}()
		// make sure tk2's ddl job already put into ddl job queue.
		time.Sleep(time.Millisecond * 100)
	})

	tk.MustExec("alter table t2 drop index idx1")
	wg.Wait()
	require.Error(t, dropErr)
	require.Equal(t, "[ddl:1553]Cannot drop index 'idx2': needed in a foreign key constraint", dropErr.Error())
}

func TestDropDatabaseWithForeignKeyReferred2(t *testing.T) {
	store := testkit.CreateMockStoreWithSchemaLease(t, testLease)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("set @@global.tidb_enable_foreign_key=1")
	tk.MustExec("set @@foreign_key_checks=1;")
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("set @@global.tidb_enable_foreign_key=1")
	tk2.MustExec("set @@foreign_key_checks=1;")
	tk2.MustExec("use test")
	tk.MustExec("create table t1 (id int key, b int, index(b));")
	tk.MustExec("create table t2 (id int key, b int, foreign key fk_b(b) references t1(id));")
	tk.MustExec("create database test2")
	var wg sync.WaitGroup
	var dropErr error
	testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/ddl/onJobRunBefore", func(job *model.Job) {
		if job.SchemaState != model.StateNone {
			return
		}
		if job.Type != model.ActionCreateTable {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			dropErr = tk2.ExecToErr("drop database test")
		}()
		// make sure tk2's ddl job already put into ddl job queue.
		time.Sleep(time.Millisecond * 100)
	})

	tk.MustExec("create table test2.t3 (id int key, b int, foreign key fk_b(b) references test.t2(id));")
	wg.Wait()
	require.Error(t, dropErr)
	require.Equal(t, "[ddl:3730]Cannot drop table 't2' referenced by a foreign key constraint 'fk_b' on table 't3'.", dropErr.Error())
	tk.MustExec("drop table test2.t3")
	tk.MustExec("drop database test")
}

func TestAddForeignKey2(t *testing.T) {
	store := testkit.CreateMockStoreWithSchemaLease(t, testLease)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("set @@global.tidb_enable_foreign_key=1")
	tk.MustExec("set @@foreign_key_checks=1;")
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("use test")
	tk.MustExec("create table t1 (id int key, b int, index(b));")
	tk.MustExec("create table t2 (id int key, b int, index(b));")
	var wg sync.WaitGroup
	var addErr error
	testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/ddl/onJobRunBefore", func(job *model.Job) {
		if job.SchemaState != model.StatePublic || job.Type != model.ActionDropIndex {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			addErr = tk2.ExecToErr("alter table t2 add foreign key (b) references t1(id);")
		}()
		// make sure tk2's ddl job already put into ddl job queue.
		time.Sleep(time.Millisecond * 100)
	})

	tk.MustExec("alter table t2 drop index b")
	wg.Wait()
	require.Error(t, addErr)
	require.Equal(t, "[ddl:-1]Failed to add the foreign key constraint. Missing index for 'fk_1' foreign key columns in the table 't2'", addErr.Error())
}

func TestAddForeignKey3(t *testing.T) {
	store := testkit.CreateMockStoreWithSchemaLease(t, testLease)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("set @@global.tidb_enable_foreign_key=1")
	tk.MustExec("set @@foreign_key_checks=1;")
	tk.MustExec("use test")
	tk2 := testkit.NewTestKit(t, store)
	tk2.MustExec("use test")
	tk2.MustExec("set @@foreign_key_checks=1;")
	tk.MustExec("create table t1 (id int key, b int, index(b));")
	tk.MustExec("create table t2 (id int, b int, index(id), index(b));")
	tk.MustExec("insert into t1 values (1, 1), (2, 2), (3, 3)")
	tk.MustExec("insert into t2 values (1, 1), (2, 2), (3, 3)")

	var insertErrs []error
	var deleteErrs []error
	testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/ddl/onJobRunBefore", func(job *model.Job) {
		if job.Type != model.ActionAddForeignKey {
			return
		}
		if job.SchemaState == model.StateWriteOnly || job.SchemaState == model.StateWriteReorganization {
			err := tk2.ExecToErr("insert into t2 values (10, 10)")
			insertErrs = append(insertErrs, err)
			err = tk2.ExecToErr("delete from t1 where id = 1")
			deleteErrs = append(deleteErrs, err)
		}
	})

	tk.MustExec("alter table t2 add foreign key (id) references t1(id) on delete cascade")
	require.Equal(t, 2, len(insertErrs))
	for _, err := range insertErrs {
		require.Error(t, err)
		require.Equal(t, "[planner:1452]Cannot add or update a child row: a foreign key constraint fails (`test`.`t2`, CONSTRAINT `fk_1` FOREIGN KEY (`id`) REFERENCES `t1` (`id`) ON DELETE CASCADE)", err.Error())
	}
	for _, err := range deleteErrs {
		require.Error(t, err)
		require.Equal(t, "[planner:1451]Cannot delete or update a parent row: a foreign key constraint fails (`test`.`t2`, CONSTRAINT `fk_1` FOREIGN KEY (`id`) REFERENCES `t1` (`id`) ON DELETE CASCADE)", err.Error())
	}
	tk.MustQuery("select * from t1 order by id").Check(testkit.Rows("1 1", "2 2", "3 3"))
	tk.MustQuery("select * from t2 order by id").Check(testkit.Rows("1 1", "2 2", "3 3"))
}

func TestForeignKeyInWriteOnlyMode(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tkDDL := testkit.NewTestKit(t, store)
	tkDDL.MustExec("use test")
	tkDDL.MustExec("create table parent (id int key)")
	tkDDL.MustExec("insert into parent values(1)")

	var notExistErrs []error
	testfailpoint.EnableCall(t, "github.com/pingcap/tidb/pkg/ddl/onJobRunBefore", func(job *model.Job) {
		if job.Type == model.ActionCreateTable && job.TableName == "child" {
			if job.SchemaState == model.StateDeleteOnly {
				// tk with the latest schema will insert data into child
				_, err := tk.Exec("insert into child values (1, 1)")
				notExistErrs = append(notExistErrs, err)
				_, err = tk.Exec("update child set id = 2 where id = 1")
				notExistErrs = append(notExistErrs, err)
				_, err = tk.Exec("delete from child where id = 1")
				notExistErrs = append(notExistErrs, err)
				_, err = tk.Exec("delete child from child inner join parent where child.pid = parent.id")
				notExistErrs = append(notExistErrs, err)
				_, err = tk.Exec("delete parent from child inner join parent where child.pid = parent.id")
				notExistErrs = append(notExistErrs, err)
			}
		}
	})
	tkDDL.MustExec("create table child (id int, pid int, index idx_pid(pid), foreign key (pid) references parent(id) on delete cascade);")

	testfailpoint.Disable(t, "github.com/pingcap/tidb/pkg/ddl/onJobRunBefore")

	for _, err := range notExistErrs {
		require.Error(t, err)
		require.Contains(t, err.Error(), "Table 'test.child' doesn't exist")
	}
}
