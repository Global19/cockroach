// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package optbuilder

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/errors"
)

// mutationBuilder is a helper struct that supports building Insert, Update,
// Upsert, and Delete operators in stages.
// TODO(andyk): Add support for Delete.
type mutationBuilder struct {
	b  *Builder
	md *opt.Metadata

	// opName is the statement's name, used in error messages.
	opName string

	// tab is the target table.
	tab cat.Table

	// tabID is the metadata ID of the table.
	tabID opt.TableID

	// alias is the table alias specified in the mutation statement, or just the
	// resolved table name if no alias was specified.
	alias tree.TableName

	// outScope contains the current set of columns that are in scope, as well as
	// the output expression as it is incrementally built. Once the final mutation
	// expression is completed, it will be contained in outScope.expr. Columns,
	// when present, are arranged in this order:
	//
	//   +--------+-------+--------+--------+-------+
	//   | Insert | Fetch | Update | Upsert | Check |
	//   +--------+-------+--------+--------+-------+
	//
	// Each column is identified by its ordinal position in outScope, and those
	// ordinals are stored in the corresponding ScopeOrds fields (see below).
	outScope *scope

	// targetColList is an ordered list of IDs of the table columns into which
	// values will be inserted, or which will be updated with new values. It is
	// incrementally built as the mutation operator is built.
	targetColList opt.ColList

	// targetColSet contains the same column IDs as targetColList, but as a set.
	targetColSet opt.ColSet

	// insertOrds lists the outScope columns providing values to insert. Its
	// length is always equal to the number of columns in the target table,
	// including mutation columns. Table columns which will not have values
	// inserted are set to -1 (e.g. delete-only mutation columns). insertOrds
	// is empty if this is not an Insert/Upsert operator.
	insertOrds []scopeOrdinal

	// fetchOrds lists the outScope columns storing values which are fetched
	// from the target table in order to provide existing values that will form
	// lookup and update values. Its length is always equal to the number of
	// columns in the target table, including mutation columns. Table columns
	// which do not need to be fetched are set to -1. fetchOrds is empty if
	// this is an Insert operator.
	fetchOrds []scopeOrdinal

	// updateOrds lists the outScope columns providing update values. Its length
	// is always equal to the number of columns in the target table, including
	// mutation columns. Table columns which do not need to be updated are set
	// to -1.
	updateOrds []scopeOrdinal

	// upsertOrds lists the outScope columns that choose between an insert or
	// update column using a CASE expression:
	//
	//   CASE WHEN canary_col IS NULL THEN ins_col ELSE upd_col END
	//
	// These columns are used to compute constraints and to return result rows.
	// The length of upsertOrds is always equal to the number of columns in
	// the target table, including mutation columns. Table columns which do not
	// need to be updated are set to -1. upsertOrds is empty if this is not
	// an Upsert operator.
	upsertOrds []scopeOrdinal

	// checkOrds lists the outScope columns storing the boolean results of
	// evaluating check constraint expressions defined on the target table. Its
	// length is always equal to the number of check constraints on the table
	// (see opt.Table.CheckCount).
	checkOrds []scopeOrdinal

	// canaryColID is the ID of the column that is used to decide whether to
	// insert or update each row. If the canary column's value is null, then it's
	// an insert; otherwise it's an update.
	canaryColID opt.ColumnID

	// subqueries temporarily stores subqueries that were built during initial
	// analysis of SET expressions. They will be used later when the subqueries
	// are joined into larger LEFT OUTER JOIN expressions.
	subqueries []*scope

	// parsedExprs is a cached set of parsed default and computed expressions
	// from the table schema. These are parsed once and cached for reuse.
	parsedExprs []tree.Expr

	// checks contains foreign key check queries; see buildFKChecks methods.
	checks memo.FKChecksExpr

	// fkFallback is true if we need to fall back on the legacy path for
	// FK checks / cascades. See buildFKChecks methods.
	fkFallback bool

	// withID is nonzero if we need to buffer the input for FK checks.
	withID opt.WithID

	// extraAccessibleCols stores all the columns that are available to the
	// mutation that are not part of the target table. This is useful for
	// UPDATE ... FROM queries, as the columns from the FROM tables must be
	// made accessible to the RETURNING clause.
	extraAccessibleCols []scopeColumn

	// fkCheckHelper is used to prevent allocating the helper separately.
	fkCheckHelper fkCheckHelper
}

func (mb *mutationBuilder) init(b *Builder, opName string, tab cat.Table, alias tree.TableName) {
	mb.b = b
	mb.md = b.factory.Metadata()
	mb.opName = opName
	mb.tab = tab
	mb.alias = alias
	mb.targetColList = make(opt.ColList, 0, tab.DeletableColumnCount())

	// Allocate segmented array of scope column ordinals.
	n := tab.DeletableColumnCount()
	scopeOrds := make([]scopeOrdinal, n*4+tab.CheckCount())
	for i := range scopeOrds {
		scopeOrds[i] = -1
	}
	mb.insertOrds = scopeOrds[:n]
	mb.fetchOrds = scopeOrds[n : n*2]
	mb.updateOrds = scopeOrds[n*2 : n*3]
	mb.upsertOrds = scopeOrds[n*3 : n*4]
	mb.checkOrds = scopeOrds[n*4:]

	// Add the table and its columns (including mutation columns) to metadata.
	mb.tabID = mb.md.AddTable(tab, &mb.alias)
}

// scopeOrdToColID returns the ID of the given scope column. If no scope column
// is defined, scopeOrdToColID returns 0.
func (mb *mutationBuilder) scopeOrdToColID(ord scopeOrdinal) opt.ColumnID {
	if ord == -1 {
		return 0
	}
	return mb.outScope.cols[ord].id
}

// insertColID is a convenience method that returns the ID of the input column
// that provides the insertion value for the given table column (specified by
// ordinal position in the table).
func (mb *mutationBuilder) insertColID(tabOrd int) opt.ColumnID {
	return mb.scopeOrdToColID(mb.insertOrds[tabOrd])
}

// buildInputForUpdate constructs a Select expression from the fields in
// the Update operator, similar to this:
//
//   SELECT <cols>
//   FROM <table>
//   WHERE <where>
//   ORDER BY <order-by>
//   LIMIT <limit>
//
// All columns from the table to update are added to fetchColList.
// If a FROM clause is defined, we build out each of the table
// expressions required and JOIN them together (LATERAL joins between
// the tables are allowed). We then JOIN the result with the target
// table (the FROM tables can't reference this table) and apply the
// appropriate WHERE conditions.
//
// It is the responsibility of the user to guarantee that the JOIN
// produces a maximum of one row per row of the target table. If multiple
// are found, an arbitrary one is chosen (this row is not readily
// predictable, consistent with the POSTGRES implementation).
// buildInputForUpdate stores the columns of the FROM tables in the
// mutation builder so they can be made accessible to other parts of
// the query (RETURNING clause).
// TODO(andyk): Do needed column analysis to project fewer columns if possible.
func (mb *mutationBuilder) buildInputForUpdate(
	inScope *scope,
	texpr tree.TableExpr,
	from tree.TableExprs,
	where *tree.Where,
	limit *tree.Limit,
	orderBy tree.OrderBy,
) {
	var indexFlags *tree.IndexFlags
	if source, ok := texpr.(*tree.AliasedTableExpr); ok {
		indexFlags = source.IndexFlags
	}

	// Fetch columns from different instance of the table metadata, so that it's
	// possible to remap columns, as in this example:
	//
	//   UPDATE abc SET a=b
	//

	// FROM
	mb.outScope = mb.b.buildScan(
		mb.b.addTable(mb.tab, &mb.alias),
		nil, /* ordinals */
		indexFlags,
		noRowLocking,
		includeMutations,
		inScope,
	)

	fromClausePresent := len(from) > 0
	numCols := len(mb.outScope.cols)

	// If there is a FROM clause present, we must join all the tables
	// together with the table being updated.
	if fromClausePresent {
		fromScope := mb.b.buildFromTables(from, noRowLocking, inScope)

		// Check that the same table name is not used multiple times.
		mb.b.validateJoinTableNames(mb.outScope, fromScope)

		// The FROM table columns can be accessed by the RETURNING clause of the
		// query and so we have to make them accessible.
		mb.extraAccessibleCols = fromScope.cols

		// Add the columns in the FROM scope.
		mb.outScope.appendColumnsFromScope(fromScope)

		left := mb.outScope.expr.(memo.RelExpr)
		right := fromScope.expr.(memo.RelExpr)
		mb.outScope.expr = mb.b.factory.ConstructInnerJoin(left, right, memo.TrueFilter, memo.EmptyJoinPrivate)
	}

	// WHERE
	mb.b.buildWhere(where, mb.outScope)

	// SELECT + ORDER BY (which may add projected expressions)
	projectionsScope := mb.outScope.replace()
	projectionsScope.appendColumnsFromScope(mb.outScope)
	orderByScope := mb.b.analyzeOrderBy(orderBy, mb.outScope, projectionsScope)
	mb.b.buildOrderBy(mb.outScope, projectionsScope, orderByScope)
	mb.b.constructProjectForScope(mb.outScope, projectionsScope)

	// LIMIT
	if limit != nil {
		mb.b.buildLimit(limit, inScope, projectionsScope)
	}

	mb.outScope = projectionsScope

	// Build a distinct on to ensure there is at most one row in the joined output
	// for every row in the table.
	if fromClausePresent {
		var pkCols opt.ColSet

		// We need to ensure that the join has a maximum of one row for every row in the
		// table and we ensure this by constructing a distinct on the primary key columns.
		primaryIndex := mb.tab.Index(cat.PrimaryIndex)
		for i := 0; i < primaryIndex.KeyColumnCount(); i++ {
			pkCol := mb.outScope.cols[primaryIndex.Column(i).Ordinal]

			// If the primary key column is hidden, then we don't need to use it
			// for the distinct on.
			if !pkCol.hidden {
				pkCols.Add(pkCol.id)
			}
		}

		if !pkCols.Empty() {
			mb.outScope = mb.b.buildDistinctOn(pkCols, mb.outScope, false /* forUpsert */)
		}
	}

	// Set list of columns that will be fetched by the input expression.
	for i := 0; i < numCols; i++ {
		mb.fetchOrds[i] = scopeOrdinal(i)
	}
}

// buildInputForDelete constructs a Select expression from the fields in
// the Delete operator, similar to this:
//
//   SELECT <cols>
//   FROM <table>
//   WHERE <where>
//   ORDER BY <order-by>
//   LIMIT <limit>
//
// All columns from the table to update are added to fetchColList.
// TODO(andyk): Do needed column analysis to project fewer columns if possible.
func (mb *mutationBuilder) buildInputForDelete(
	inScope *scope, texpr tree.TableExpr, where *tree.Where, limit *tree.Limit, orderBy tree.OrderBy,
) {
	var indexFlags *tree.IndexFlags
	if source, ok := texpr.(*tree.AliasedTableExpr); ok {
		indexFlags = source.IndexFlags
	}

	// Fetch columns from different instance of the table metadata, so that it's
	// possible to remap columns, as in this example:
	//
	//   DELETE FROM abc WHERE a=b
	//
	mb.outScope = mb.b.buildScan(
		mb.b.addTable(mb.tab, &mb.alias),
		nil, /* ordinals */
		indexFlags,
		noRowLocking,
		includeMutations,
		inScope,
	)

	// WHERE
	mb.b.buildWhere(where, mb.outScope)

	// SELECT + ORDER BY (which may add projected expressions)
	projectionsScope := mb.outScope.replace()
	projectionsScope.appendColumnsFromScope(mb.outScope)
	orderByScope := mb.b.analyzeOrderBy(orderBy, mb.outScope, projectionsScope)
	mb.b.buildOrderBy(mb.outScope, projectionsScope, orderByScope)
	mb.b.constructProjectForScope(mb.outScope, projectionsScope)

	// LIMIT
	if limit != nil {
		mb.b.buildLimit(limit, inScope, projectionsScope)
	}

	mb.outScope = projectionsScope

	// Set list of columns that will be fetched by the input expression.
	for i := range mb.outScope.cols {
		mb.fetchOrds[i] = scopeOrdinal(i)
	}
}

// addTargetColsByName adds one target column for each of the names in the given
// list.
func (mb *mutationBuilder) addTargetColsByName(names tree.NameList) {
	for _, name := range names {
		// Determine the ordinal position of the named column in the table and
		// add it as a target column.
		if ord := cat.FindTableColumnByName(mb.tab, name); ord != -1 {
			mb.addTargetCol(ord)
			continue
		}
		panic(sqlbase.NewUndefinedColumnError(string(name)))
	}
}

// addTargetCol adds a target column by its ordinal position in the target
// table. It raises an error if a mutation or computed column is targeted, or if
// the same column is targeted multiple times.
func (mb *mutationBuilder) addTargetCol(ord int) {
	tabCol := mb.tab.Column(ord)

	// Don't allow targeting of mutation columns.
	if cat.IsMutationColumn(mb.tab, ord) {
		panic(makeBackfillError(tabCol.ColName()))
	}

	// Computed columns cannot be targeted with input values.
	if tabCol.IsComputed() {
		panic(sqlbase.CannotWriteToComputedColError(string(tabCol.ColName())))
	}

	// Ensure that the name list does not contain duplicates.
	colID := mb.tabID.ColumnID(ord)
	if mb.targetColSet.Contains(colID) {
		panic(pgerror.Newf(pgcode.Syntax,
			"multiple assignments to the same column %q", tabCol.ColName()))
	}
	mb.targetColSet.Add(colID)

	mb.targetColList = append(mb.targetColList, colID)
}

// extractValuesInput tests whether the given input is a VALUES clause with no
// WITH, ORDER BY, or LIMIT modifier. If so, it's returned, otherwise nil is
// returned.
func (mb *mutationBuilder) extractValuesInput(inputRows *tree.Select) *tree.ValuesClause {
	if inputRows == nil {
		return nil
	}

	// Only extract a simple VALUES clause with no modifiers.
	if inputRows.With != nil || inputRows.OrderBy != nil || inputRows.Limit != nil {
		return nil
	}

	// Discard parentheses.
	if parens, ok := inputRows.Select.(*tree.ParenSelect); ok {
		return mb.extractValuesInput(parens.Select)
	}

	if values, ok := inputRows.Select.(*tree.ValuesClause); ok {
		return values
	}

	return nil
}

// replaceDefaultExprs looks for DEFAULT specifiers in input value expressions
// and replaces them with the corresponding default value expression for the
// corresponding column. This is only possible when the input is a VALUES
// clause. For example:
//
//   INSERT INTO t (a, b) (VALUES (1, DEFAULT), (DEFAULT, 2))
//
// Here, the two DEFAULT specifiers are replaced by the default value expression
// for the a and b columns, respectively.
//
// replaceDefaultExprs returns a VALUES expression with replaced DEFAULT values,
// or just the unchanged input expression if there are no DEFAULT values.
func (mb *mutationBuilder) replaceDefaultExprs(inRows *tree.Select) (outRows *tree.Select) {
	values := mb.extractValuesInput(inRows)
	if values == nil {
		return inRows
	}

	// Ensure that the number of input columns exactly matches the number of
	// target columns.
	numCols := len(values.Rows[0])
	mb.checkNumCols(len(mb.targetColList), numCols)

	var newRows []tree.Exprs
	for irow, tuple := range values.Rows {
		if len(tuple) != numCols {
			reportValuesLenError(numCols, len(tuple))
		}

		// Scan list of tuples in the VALUES row, looking for DEFAULT specifiers.
		var newTuple tree.Exprs
		for itup, val := range tuple {
			if _, ok := val.(tree.DefaultVal); ok {
				// Found DEFAULT, so lazily create new rows and tuple lists.
				if newRows == nil {
					newRows = make([]tree.Exprs, irow, len(values.Rows))
					copy(newRows, values.Rows[:irow])
				}

				if newTuple == nil {
					newTuple = make(tree.Exprs, itup, numCols)
					copy(newTuple, tuple[:itup])
				}

				val = mb.parseDefaultOrComputedExpr(mb.targetColList[itup])
			}
			if newTuple != nil {
				newTuple = append(newTuple, val)
			}
		}

		if newRows != nil {
			if newTuple != nil {
				newRows = append(newRows, newTuple)
			} else {
				newRows = append(newRows, tuple)
			}
		}
	}

	if newRows != nil {
		return &tree.Select{Select: &tree.ValuesClause{Rows: newRows}}
	}
	return inRows
}

// addSynthesizedCols is a helper method for addDefaultAndComputedColsForInsert
// and addComputedColsForUpdate that scans the list of table columns, looking
// for any that do not yet have values provided by the input expression. New
// columns are synthesized for any missing columns, as long as the addCol
// callback function returns true for that column.
func (mb *mutationBuilder) addSynthesizedCols(
	scopeOrds []scopeOrdinal, addCol func(tabCol cat.Column) bool,
) {
	var projectionsScope *scope

	// Skip delete-only mutation columns, since they are ignored by all mutation
	// operators that synthesize columns.
	for i, n := 0, mb.tab.WritableColumnCount(); i < n; i++ {
		// Skip columns that are already specified.
		if scopeOrds[i] != -1 {
			continue
		}

		// Invoke addCol to determine whether column should be added.
		tabCol := mb.tab.Column(i)
		if !addCol(tabCol) {
			continue
		}

		// Construct a new Project operator that will contain the newly synthesized
		// column(s).
		if projectionsScope == nil {
			projectionsScope = mb.outScope.replace()
			projectionsScope.appendColumnsFromScope(mb.outScope)
		}
		tabColID := mb.tabID.ColumnID(i)
		expr := mb.parseDefaultOrComputedExpr(tabColID)
		texpr := mb.outScope.resolveAndRequireType(expr, tabCol.DatumType())
		scopeCol := mb.b.addColumn(projectionsScope, "" /* alias */, texpr)
		mb.b.buildScalar(texpr, mb.outScope, projectionsScope, scopeCol, nil)

		// Assign name to synthesized column. Computed columns may refer to default
		// columns in the table by name.
		scopeCol.name = tabCol.ColName()

		// Remember ordinal position of the new scope column.
		scopeOrds[i] = scopeOrdinal(len(projectionsScope.cols) - 1)

		// Add corresponding target column.
		mb.targetColList = append(mb.targetColList, tabColID)
		mb.targetColSet.Add(tabColID)
	}

	if projectionsScope != nil {
		mb.b.constructProjectForScope(mb.outScope, projectionsScope)
		mb.outScope = projectionsScope
	}
}

// roundDecimalValues wraps each DECIMAL-related column (including arrays of
// decimals) with a call to the crdb_internal.round_decimal_values function, if
// column values may need to be rounded. This is necessary when mutating table
// columns that have a limited scale (e.g. DECIMAL(10, 1)). Here is the PG docs
// description:
//
//   http://www.postgresql.org/docs/9.5/static/datatype-numeric.html
//   "If the scale of a value to be stored is greater than
//   the declared scale of the column, the system will round the
//   value to the specified number of fractional digits. Then,
//   if the number of digits to the left of the decimal point
//   exceeds the declared precision minus the declared scale, an
//   error is raised."
//
// Note that this function only handles the rounding portion of that. The
// precision check is done by the execution engine. The rounding cannot be done
// there, since it needs to happen before check constraints are computed, and
// before UPSERT joins.
//
// if roundComputedCols is false, then don't wrap computed columns. If true,
// then only wrap computed columns. This is necessary because computed columns
// can depend on other columns mutated by the operation; it is necessary to
// first round those values, then evaluated the computed expression, and then
// round the result of the computation.
func (mb *mutationBuilder) roundDecimalValues(scopeOrds []scopeOrdinal, roundComputedCols bool) {
	var projectionsScope *scope

	for i, ord := range scopeOrds {
		if ord == -1 {
			// Column not mutated, so nothing to do.
			continue
		}

		// Include or exclude computed columns, depending on the value of
		// roundComputedCols.
		col := mb.tab.Column(i)
		if col.IsComputed() != roundComputedCols {
			continue
		}

		// Check whether the target column's type may require rounding of the
		// input value.
		props, overload := findRoundingFunction(col.DatumType(), col.ColTypePrecision())
		if props == nil {
			continue
		}
		private := &memo.FunctionPrivate{
			Name:       "crdb_internal.round_decimal_values",
			Typ:        mb.outScope.cols[ord].typ,
			Properties: props,
			Overload:   overload,
		}
		variable := mb.b.factory.ConstructVariable(mb.scopeOrdToColID(ord))
		scale := mb.b.factory.ConstructConstVal(tree.NewDInt(tree.DInt(col.ColTypeWidth())), types.Int)
		fn := mb.b.factory.ConstructFunction(memo.ScalarListExpr{variable, scale}, private)

		// Lazily create new scope and update the scope column to be rounded.
		if projectionsScope == nil {
			projectionsScope = mb.outScope.replace()
			projectionsScope.appendColumnsFromScope(mb.outScope)
		}
		mb.b.populateSynthesizedColumn(&projectionsScope.cols[ord], fn)
	}

	if projectionsScope != nil {
		mb.b.constructProjectForScope(mb.outScope, projectionsScope)
		mb.outScope = projectionsScope
	}
}

// findRoundingFunction returns the builtin function overload needed to round
// input values. This is only necessary for DECIMAL or DECIMAL[] types that have
// limited precision, such as:
//
//   DECIMAL(15, 1)
//   DECIMAL(10, 3)[]
//
// If an input decimal value has more than the required number of fractional
// digits, it must be rounded before being inserted into these types.
//
// NOTE: CRDB does not allow nested array storage types, so only one level of
// array nesting needs to be checked.
func findRoundingFunction(typ *types.T, precision int) (*tree.FunctionProperties, *tree.Overload) {
	if precision == 0 {
		// Unlimited precision decimal target type never needs rounding.
		return nil, nil
	}

	props, overloads := builtins.GetBuiltinProperties("crdb_internal.round_decimal_values")

	if typ.Equivalent(types.Decimal) {
		return props, &overloads[0]
	}
	if typ.Equivalent(types.DecimalArray) {
		return props, &overloads[1]
	}

	// Not DECIMAL or DECIMAL[].
	return nil, nil
}

// addCheckConstraintCols synthesizes a boolean output column for each check
// constraint defined on the target table. The mutation operator will report
// a constraint violation error if the value of the column is false.
func (mb *mutationBuilder) addCheckConstraintCols() {
	if mb.tab.CheckCount() != 0 {
		// Disambiguate names so that references in the constraint expression refer
		// to the correct columns.
		mb.disambiguateColumns()

		projectionsScope := mb.outScope.replace()
		projectionsScope.appendColumnsFromScope(mb.outScope)

		for i, n := 0, mb.tab.CheckCount(); i < n; i++ {
			expr, err := parser.ParseExpr(mb.tab.Check(i).Constraint)
			if err != nil {
				panic(err)
			}

			alias := fmt.Sprintf("check%d", i+1)
			texpr := mb.outScope.resolveAndRequireType(expr, types.Bool)
			scopeCol := mb.b.addColumn(projectionsScope, alias, texpr)

			// TODO(ridwanmsharif): Maybe we can avoid building constraints here
			// and instead use the constraints stored in the table metadata.
			mb.b.buildScalar(texpr, mb.outScope, projectionsScope, scopeCol, nil)
			mb.checkOrds[i] = scopeOrdinal(len(projectionsScope.cols) - 1)
		}

		mb.b.constructProjectForScope(mb.outScope, projectionsScope)
		mb.outScope = projectionsScope
	}
}

// disambiguateColumns ranges over the scope and ensures that at most one column
// has each table column name, and that name refers to the column with the final
// value that the mutation applies.
func (mb *mutationBuilder) disambiguateColumns() {
	// Determine the set of scope columns that will have their names preserved.
	var preserve util.FastIntSet
	for i, n := 0, mb.tab.DeletableColumnCount(); i < n; i++ {
		scopeOrd := mb.mapToReturnScopeOrd(i)
		if scopeOrd != -1 {
			preserve.Add(int(scopeOrd))
		}
	}

	// Clear names of all non-preserved columns.
	for i := range mb.outScope.cols {
		if !preserve.Contains(i) {
			mb.outScope.cols[i].clearName()
		}
	}
}

// makeMutationPrivate builds a MutationPrivate struct containing the table and
// column metadata needed for the mutation operator.
func (mb *mutationBuilder) makeMutationPrivate(needResults bool) *memo.MutationPrivate {
	// Helper function to create a column list in the MutationPrivate.
	makeColList := func(scopeOrds []scopeOrdinal) opt.ColList {
		var colList opt.ColList
		for i := range scopeOrds {
			if scopeOrds[i] != -1 {
				if colList == nil {
					colList = make(opt.ColList, len(scopeOrds))
				}
				colList[i] = mb.scopeOrdToColID(scopeOrds[i])
			}
		}
		return colList
	}

	private := &memo.MutationPrivate{
		Table:      mb.tabID,
		InsertCols: makeColList(mb.insertOrds),
		FetchCols:  makeColList(mb.fetchOrds),
		UpdateCols: makeColList(mb.updateOrds),
		CanaryCol:  mb.canaryColID,
		CheckCols:  makeColList(mb.checkOrds),
		FKFallback: mb.fkFallback,
	}

	// If we didn't actually plan any checks (e.g. because of cascades), don't
	// buffer the input.
	if len(mb.checks) > 0 {
		private.WithID = mb.withID
	}

	if needResults {
		// Only non-mutation columns are output columns. ReturnCols needs to have
		// DeletableColumnCount entries, but only the first ColumnCount entries
		// can be defined (i.e. >= 0).
		private.ReturnCols = make(opt.ColList, mb.tab.DeletableColumnCount())
		for i, n := 0, mb.tab.ColumnCount(); i < n; i++ {
			scopeOrd := mb.mapToReturnScopeOrd(i)
			if scopeOrd == -1 {
				panic(errors.AssertionFailedf("column %d is not available in the mutation input", i))
			}
			private.ReturnCols[i] = mb.outScope.cols[scopeOrd].id
		}
	}

	return private
}

// mapToReturnScopeOrd returns the ordinal of the scope column that provides the
// final value for the column at the given ordinal position in the table. This
// value might mutate the column, or it might be returned by the mutation
// statement, or it might not be used at all. Columns take priority in this
// order:
//
//   upsert, update, fetch, insert
//
// If an upsert column is available, then it already combines an update/fetch
// value with an insert value, so it takes priority. If an update column is
// available, then it overrides any fetch value. Finally, the relative priority
// of fetch and insert columns doesn't matter, since they're only used together
// in the upsert case where an upsert column would be available.
//
// If the column is never referenced by the statement, then mapToReturnScopeOrd
// returns 0. This would be the case for delete-only columns in an Insert
// statement, because they're neither fetched nor mutated.
func (mb *mutationBuilder) mapToReturnScopeOrd(tabOrd int) scopeOrdinal {
	switch {
	case mb.upsertOrds[tabOrd] != -1:
		return mb.upsertOrds[tabOrd]

	case mb.updateOrds[tabOrd] != -1:
		return mb.updateOrds[tabOrd]

	case mb.fetchOrds[tabOrd] != -1:
		return mb.fetchOrds[tabOrd]

	case mb.insertOrds[tabOrd] != -1:
		return mb.insertOrds[tabOrd]

	default:
		// Column is never referenced by the statement.
		return -1
	}
}

// buildReturning wraps the input expression with a Project operator that
// projects the given RETURNING expressions.
func (mb *mutationBuilder) buildReturning(returning tree.ReturningExprs) {
	// Handle case of no RETURNING clause.
	if returning == nil {
		mb.outScope = &scope{builder: mb.b, expr: mb.outScope.expr}
		return
	}

	// Start out by constructing a scope containing one column for each non-
	// mutation column in the target table, in the same order, and with the
	// same names. These columns can be referenced by the RETURNING clause.
	//
	//   1. Project only non-mutation columns.
	//   2. Alias columns to use table column names.
	//   3. Mark hidden columns.
	//   4. Project columns in same order as defined in table schema.
	//
	inScope := mb.outScope.replace()
	inScope.expr = mb.outScope.expr
	inScope.appendColumnsFromTable(mb.md.TableMeta(mb.tabID), &mb.alias)

	// extraAccessibleCols contains all the columns that the RETURNING
	// clause can refer to in addition to the table columns. This is useful for
	// UPDATE ... FROM statements, where all columns from tables in the FROM clause
	// are in scope for the RETURNING clause.
	inScope.appendColumns(mb.extraAccessibleCols)

	// Construct the Project operator that projects the RETURNING expressions.
	outScope := inScope.replace()
	mb.b.analyzeReturningList(returning, nil /* desiredTypes */, inScope, outScope)
	mb.b.buildProjectionList(inScope, outScope)
	mb.b.constructProjectForScope(inScope, outScope)
	mb.outScope = outScope
}

// checkNumCols raises an error if the expected number of columns does not match
// the actual number of columns.
func (mb *mutationBuilder) checkNumCols(expected, actual int) {
	if actual != expected {
		more, less := "expressions", "target columns"
		if actual < expected {
			more, less = less, more
		}

		panic(pgerror.Newf(pgcode.Syntax,
			"%s has more %s than %s, %d expressions for %d targets",
			strings.ToUpper(mb.opName), more, less, actual, expected))
	}
}

// parseDefaultOrComputedExpr parses the default (including nullable) or
// computed value expression for the given table column, and caches it for
// reuse.
func (mb *mutationBuilder) parseDefaultOrComputedExpr(colID opt.ColumnID) tree.Expr {
	if mb.parsedExprs == nil {
		mb.parsedExprs = make([]tree.Expr, mb.tab.DeletableColumnCount())
	}

	// Return expression from cache, if it was already parsed previously.
	ord := mb.tabID.ColumnOrdinal(colID)
	if mb.parsedExprs[ord] != nil {
		return mb.parsedExprs[ord]
	}

	var exprStr string
	tabCol := mb.tab.Column(ord)
	switch {
	case tabCol.IsComputed():
		exprStr = tabCol.ComputedExprStr()
	case tabCol.HasDefault():
		exprStr = tabCol.DefaultExprStr()
	default:
		return tree.DNull
	}

	expr, err := parser.ParseExpr(exprStr)
	if err != nil {
		panic(err)
	}

	mb.parsedExprs[ord] = expr
	return expr
}

// buildFKChecks* methods populate mb.checks with queries that check the
// integrity of foreign key relations that involve modified rows.
//
// The foreign key checks are queries that run after the statement (including
// the relevant mutation) completes; any row that is returned by these
// FK check queries indicates a foreign key violation.
//
// In the case of insert, each FK check query is an anti-join with the left side
// being a WithScan of the mutation input and the right side being the
// referenced table. A simple example of an insert with a FK check:
//
//   insert child
//    ├── ...
//    ├── input binding: &1
//    └── f-k-checks
//         └── f-k-checks-item: child(p) -> parent(p)
//              └── anti-join (hash)
//                   ├── columns: column2:5!null
//                   ├── with-scan &1
//                   │    ├── columns: column2:5!null
//                   │    └── mapping:
//                   │         └──  column2:4 => column2:5
//                   ├── scan parent
//                   │    └── columns: parent.p:6!null
//                   └── filters
//                        └── column2:5 = parent.p:6
//
// See testdata/fk-checks-insert for more examples.
//
func (mb *mutationBuilder) buildFKChecksForInsert() {
	if mb.tab.OutboundForeignKeyCount() == 0 {
		// No relevant FKs.
		return
	}
	if !mb.b.evalCtx.SessionData.OptimizerFKs {
		mb.fkFallback = true
		return
	}

	// TODO(radu): if the input is a VALUES with constant expressions, we don't
	// need to buffer it. This could be a normalization rule, but it's probably
	// more efficient if we did it in here (or we'd end up building the entire FK
	// subtrees twice).
	mb.withID = mb.b.factory.Memo().NextWithID()

	for i, n := 0, mb.tab.OutboundForeignKeyCount(); i < n; i++ {
		mb.addInsertionCheck(i)
	}
}

// buildFKChecks* methods populate mb.checks with queries that check the
// integrity of foreign key relations that involve modified rows.
//
// The foreign key checks are queries that run after the statement (including
// the relevant mutation) completes; any row that is returned by these
// FK check queries indicates a foreign key violation.
//
// In the case of delete, each FK check query is a semi-join with the left side
// being a WithScan of the mutation input and the right side being the
// referencing table. For example:
//   delete parent
//    ├── ...
//    ├── input binding: &1
//    └── f-k-checks
//         └── f-k-checks-item: child(p) -> parent(p)
//              └── semi-join (hash)
//                   ├── columns: p:7!null
//                   ├── with-scan &1
//                   │    ├── columns: p:7!null
//                   │    └── mapping:
//                   │         └──  parent.p:5 => p:7
//                   ├── scan child
//                   │    └── columns: child.p:9!null
//                   └── filters
//                        └── p:7 = child.p:9
//
// See testdata/fk-checks-delete for more examples.
//
func (mb *mutationBuilder) buildFKChecksForDelete() {
	if mb.tab.InboundForeignKeyCount() == 0 {
		// No relevant FKs.
		return
	}
	if !mb.b.evalCtx.SessionData.OptimizerFKs {
		mb.fkFallback = true
		return
	}

	mb.withID = mb.b.factory.Memo().NextWithID()

	for i, n := 0, mb.tab.InboundForeignKeyCount(); i < n; i++ {
		h := &mb.fkCheckHelper
		if !h.initWithInboundFK(mb, i) {
			continue
		}

		if a := h.fk.DeleteReferenceAction(); a != tree.Restrict && a != tree.NoAction {
			// Bail, so that exec FK checks pick up on FK checks and perform them.
			mb.checks = nil
			mb.fkFallback = true
			return
		}

		fkInput, withScanCols, _ := h.makeFKInputScan(fkInputScanFetchedVals)
		mb.addDeletionCheck(h, fkInput, withScanCols)
	}
}

// buildFKChecks* methods populate mb.checks with queries that check the
// integrity of foreign key relations that involve modified rows.
//
// The foreign key checks are queries that run after the statement (including
// the relevant mutation) completes; any row that is returned by these
// FK check queries indicates a foreign key violation.
//
// In the case of update, there are two types of FK check queries:
//
//  - insertion-side checks are very similar to the checks we issue for insert;
//    they are an anti-join with the left side being a WithScan of the "new"
//    values for each row. For example:
//      update child
//       ├── ...
//       ├── input binding: &1
//       └── f-k-checks
//            └── f-k-checks-item: child(p) -> parent(p)
//                 └── anti-join (hash)
//                      ├── columns: column5:6!null
//                      ├── with-scan &1
//                      │    ├── columns: column5:6!null
//                      │    └── mapping:
//                      │         └──  column5:5 => column5:6
//                      ├── scan parent
//                      │    └── columns: parent.p:8!null
//                      └── filters
//                           └── column5:6 = parent.p:8
//
//  - deletion-side checks are similar to the checks we issue for delete; they
//    are a semi-join but the left side input is more complicated: it is an
//    Except between a WithScan of the "old" values and a WithScan of the "new"
//    values for each row (this is the set of values that are effectively
//    removed from the table). For example:
//      update parent
//       ├── ...
//       ├── input binding: &1
//       └── f-k-checks
//            └── f-k-checks-item: child(p) -> parent(p)
//                 └── semi-join (hash)
//                      ├── columns: p:8!null
//                      ├── except
//                      │    ├── columns: p:8!null
//                      │    ├── left columns: p:8!null
//                      │    ├── right columns: column7:9
//                      │    ├── with-scan &1
//                      │    │    ├── columns: p:8!null
//                      │    │    └── mapping:
//                      │    │         └──  parent.p:5 => p:8
//                      │    └── with-scan &1
//                      │         ├── columns: column7:9!null
//                      │         └── mapping:
//                      │              └──  column7:7 => column7:9
//                      ├── scan child
//                      │    └── columns: child.p:11!null
//                      └── filters
//                           └── p:8 = child.p:11
//
// Only FK relations that involve updated columns result in FK checks.
//
func (mb *mutationBuilder) buildFKChecksForUpdate() {
	if mb.tab.OutboundForeignKeyCount() == 0 && mb.tab.InboundForeignKeyCount() == 0 {
		return
	}
	if !mb.b.evalCtx.SessionData.OptimizerFKs {
		mb.fkFallback = true
		return
	}

	mb.withID = mb.b.factory.Memo().NextWithID()

	// An Update can be thought of an insertion paired with a deletion, so for an
	// Update we can emit both semi-joins and anti-joins.

	// Each row input to the Update operator contains both the existing and the
	// new value for each updated column. From this we can construct the effective
	// insertion and deletion.

	// Say the table being updated by an update is:
	//
	//   x | y | z
	//   --+---+--
	//   1 | 3 | 5
	//
	// And we are executing UPDATE t SET y = 10, then the input to the Update
	// operator will look like:
	//
	//   x | y | z | new_y
	//   --+---+---+------
	//   1 | 3 | 5 |  10
	//
	// The insertion check will happen on the "new" row (x, new_y, z); the deletion
	// check will happen on the "old" row (x, y, z).

	for i, n := 0, mb.tab.OutboundForeignKeyCount(); i < n; i++ {
		// Verify that at least one FK column is actually updated.
		if mb.outboundFKColsUpdated(i) {
			mb.addInsertionCheck(i)
		}
	}

	// The "deletion" incurred by an update is the rows deleted for a given
	// inbound FK minus the rows inserted.
	for i, n := 0, mb.tab.InboundForeignKeyCount(); i < n; i++ {
		// Verify that at least one FK column is actually updated.
		if !mb.inboundFKColsUpdated(i) {
			continue
		}
		h := &mb.fkCheckHelper
		if !h.initWithInboundFK(mb, i) {
			// The FK constraint can safely be ignored.
			continue
		}

		if a := h.fk.UpdateReferenceAction(); a != tree.Restrict && a != tree.NoAction {
			// Bail, so that exec FK checks pick up on FK checks and perform them.
			mb.checks = nil
			mb.fkFallback = true
			return
		}

		// Construct an Except expression for the set difference between "old"
		// FK values and "new" FK values.
		//
		// The simplest example to see why this is necessary is when we are
		// "updating" a value to the same value, e.g:
		//   UPDATE child SET c = c
		// Here we are not removing any values from the column, so we must not
		// check for orphaned rows or we will be generating bogus FK violation
		// errors.
		//
		// There are more complicated cases where one row replaces the value from
		// another row, e.g.
		//   UPDATE child SET c = c+1
		// when we have existing consecutive values. These cases are sketchy because
		// depending on the order in which the mutations are applied, they may or
		// may not result in unique index violations (but if they go through, the FK
		// checks should be accurate).
		//
		// Note that the same reasoning could be applied to the insertion checks,
		// but in that case, it is not a correctness issue: it's always ok to
		// recheck that an existing row is not orphan. It's not really desirable for
		// performance either: we would be incurring extra cost (more complicated
		// expressions, scanning the input buffer twice) for a rare case.

		oldRows, colsForOldRow, _ := h.makeFKInputScan(fkInputScanFetchedVals)
		newRows, colsForNewRow, _ := h.makeFKInputScan(fkInputScanNewVals)

		// The rows that no longer exist are the ones that were "deleted" by virtue
		// of being updated _from_, minus the ones that were "added" by virtue of
		// being updated _to_.
		deletedRows := mb.b.factory.ConstructExcept(
			oldRows,
			newRows,
			&memo.SetPrivate{
				LeftCols:  colsForOldRow,
				RightCols: colsForNewRow,
				OutCols:   colsForOldRow,
			},
		)

		mb.addDeletionCheck(h, deletedRows, colsForOldRow)
	}
}

// buildFKChecks* methods populate mb.checks with queries that check the
// integrity of foreign key relations that involve modified rows.
//
// The foreign key checks are queries that run after the statement (including
// the relevant mutation) completes; any row that is returned by these
// FK check queries indicates a foreign key violation.
//
// The case of upsert is very similar to update; see buildFKChecksForUpdate.
// The main difference is that for update, the "new" values were readily
// available, whereas for upsert, the "new" values can be the result of an
// expression of the form:
//   CASE WHEN canary IS NULL THEN inserter-value ELSE updated-value END
// These expressions are already projected as part of the mutation input and are
// directly accessible through WithScan.
//
// Only FK relations that involve updated columns result in deletion-side FK
// checks. The insertion-side FK checks are always needed (similar to insert)
// because any of the rows might result in an insert rather than an update.
//
func (mb *mutationBuilder) buildFKChecksForUpsert() {
	numOutbound := mb.tab.OutboundForeignKeyCount()
	numInbound := mb.tab.InboundForeignKeyCount()

	if numOutbound == 0 && numInbound == 0 {
		return
	}

	if !mb.b.evalCtx.SessionData.OptimizerFKs {
		mb.fkFallback = true
		return
	}

	mb.withID = mb.b.factory.Memo().NextWithID()

	for i := 0; i < numOutbound; i++ {
		mb.addInsertionCheck(i)
	}

	for i := 0; i < numInbound; i++ {
		// Verify that at least one FK column is updated by the Upsert; columns that
		// are not updated can get new values (through the insert path) but existing
		// values are never removed.
		if !mb.inboundFKColsUpdated(i) {
			continue
		}

		h := &mb.fkCheckHelper
		if !h.initWithInboundFK(mb, i) {
			continue
		}

		if a := h.fk.UpdateReferenceAction(); a != tree.Restrict && a != tree.NoAction {
			// Bail, so that exec FK checks pick up on FK checks and perform them.
			mb.checks = nil
			mb.fkFallback = true
			return
		}

		// Construct an Except expression for the set difference between "old" FK
		// values and "new" FK values. See buildFKChecksForUpdate for more details.
		//
		// Note that technically, to get "old" values for the updated rows we should
		// be selecting only the rows that correspond to updates, as opposed to
		// insertions (using a "canaryCol IS NOT NULL" condition). But the rows we
		// would filter out have all-null fetched values anyway and will never match
		// in the semi join.
		oldRows, colsForOldRow, _ := h.makeFKInputScan(fkInputScanFetchedVals)
		newRows, colsForNewRow, _ := h.makeFKInputScan(fkInputScanNewVals)

		// The rows that no longer exist are the ones that were "deleted" by virtue
		// of being updated _from_, minus the ones that were "added" by virtue of
		// being updated _to_.
		deletedRows := mb.b.factory.ConstructExcept(
			oldRows,
			newRows,
			&memo.SetPrivate{
				LeftCols:  colsForOldRow,
				RightCols: colsForNewRow,
				OutCols:   colsForOldRow,
			},
		)
		mb.addDeletionCheck(h, deletedRows, colsForOldRow)
	}
}

// addInsertionCheck adds a FK check for rows which are added to a table.
// The input to the insertion check will be produced from the input to the
// mutation operator.
func (mb *mutationBuilder) addInsertionCheck(fkOrdinal int) {
	h := &mb.fkCheckHelper
	h.initWithOutboundFK(mb, fkOrdinal)

	fkInput, withScanCols, notNullWithScanCols := h.makeFKInputScan(fkInputScanNewVals)

	numCols := len(withScanCols)
	if notNullWithScanCols.Len() < numCols {
		// The columns we are inserting might have NULLs. These require special
		// handling, depending on the match method:
		//  - MATCH SIMPLE: allows any column(s) to be NULL and the row doesn't
		//                  need to have a match in the referenced table.
		//  - MATCH FULL: only the case where *all* the columns are NULL is
		//                allowed, and the row doesn't need to have a match in the
		//                referenced table.
		//
		// Note that rows that have NULLs will never have a match in the anti
		// join and will generate errors. To handle these cases, we filter the
		// mutated rows (before the anti join) to remove those which don't need a
		// match.
		//
		// For SIMPLE, we filter out any rows which have a NULL. For FULL, we
		// filter out any rows where all the columns are NULL (rows which have
		// NULLs a subset of columns are let through and will generate FK errors
		// because they will never have a match in the anti join).
		switch m := h.fk.MatchMethod(); m {
		case tree.MatchSimple:
			// Filter out any rows which have a NULL; build filters of the form
			//   (a IS NOT NULL) AND (b IS NOT NULL) ...
			filters := make(memo.FiltersExpr, 0, numCols-notNullWithScanCols.Len())
			for _, col := range withScanCols {
				if !notNullWithScanCols.Contains(col) {
					filters = append(filters, mb.b.factory.ConstructFiltersItem(
						mb.b.factory.ConstructIsNot(
							mb.b.factory.ConstructVariable(col),
							memo.NullSingleton,
						),
					))
				}
			}
			fkInput = mb.b.factory.ConstructSelect(fkInput, filters)

		case tree.MatchFull:
			// Filter out any rows which have NULLs on all referencing columns.
			if !notNullWithScanCols.Empty() {
				// We statically know that some of the referencing columns can't be
				// NULL. In this case, we don't need to filter anything (the case
				// where all the origin columns are NULL is not possible).
				break
			}
			// Build a filter of the form
			//   (a IS NOT NULL) OR (b IS NOT NULL) ...
			var condition opt.ScalarExpr
			for _, col := range withScanCols {
				is := mb.b.factory.ConstructIsNot(
					mb.b.factory.ConstructVariable(col),
					memo.NullSingleton,
				)
				if condition == nil {
					condition = is
				} else {
					condition = mb.b.factory.ConstructOr(condition, is)
				}
			}
			fkInput = mb.b.factory.ConstructSelect(
				fkInput,
				memo.FiltersExpr{mb.b.factory.ConstructFiltersItem(condition)},
			)

		default:
			panic(errors.AssertionFailedf("match method %s not supported", m))
		}
	}

	// Build an anti-join, with the origin FK columns on the left and the
	// referenced columns on the right.

	scanScope, refTabMeta := h.buildOtherTableScan()

	// Build the join filters:
	//   (origin_a = referenced_a) AND (origin_b = referenced_b) AND ...
	antiJoinFilters := make(memo.FiltersExpr, numCols)
	for j := 0; j < numCols; j++ {
		antiJoinFilters[j] = mb.b.factory.ConstructFiltersItem(
			mb.b.factory.ConstructEq(
				mb.b.factory.ConstructVariable(withScanCols[j]),
				mb.b.factory.ConstructVariable(scanScope.cols[j].id),
			),
		)
	}
	antiJoin := mb.b.factory.ConstructAntiJoin(
		fkInput, scanScope.expr, antiJoinFilters, &memo.JoinPrivate{},
	)

	check := mb.b.factory.ConstructFKChecksItem(antiJoin, &memo.FKChecksItemPrivate{
		OriginTable:     mb.tabID,
		ReferencedTable: refTabMeta.MetaID,
		FKOutbound:      true,
		FKOrdinal:       fkOrdinal,
		KeyCols:         withScanCols,
		OpName:          mb.opName,
	})

	mb.checks = append(mb.checks, check)
}

// addDeletionCheck adds a FK check for rows which are removed from a table.
// deletedRows is used as the input to the deletion check, and deleteCols is a
// list of the columns for the rows being deleted, containing values for the
// referenced FK columns in the table we are mutating.
func (mb *mutationBuilder) addDeletionCheck(
	h *fkCheckHelper, deletedRows memo.RelExpr, deleteCols opt.ColList,
) {
	// Build a semi join, with the referenced FK columns on the left and the
	// origin columns on the right.
	scanScope, origTabMeta := h.buildOtherTableScan()

	// Note that it's impossible to orphan a row whose FK key columns contain a
	// NULL, since by definition a NULL never refers to an actual row (in
	// either MATCH FULL or MATCH SIMPLE).
	// Build the join filters:
	//   (origin_a = referenced_a) AND (origin_b = referenced_b) AND ...
	semiJoinFilters := make(memo.FiltersExpr, len(deleteCols))
	for j := range deleteCols {
		semiJoinFilters[j] = mb.b.factory.ConstructFiltersItem(
			mb.b.factory.ConstructEq(
				mb.b.factory.ConstructVariable(deleteCols[j]),
				mb.b.factory.ConstructVariable(scanScope.cols[j].id),
			),
		)
	}
	semiJoin := mb.b.factory.ConstructSemiJoin(
		deletedRows, scanScope.expr, semiJoinFilters, &memo.JoinPrivate{},
	)

	check := mb.b.factory.ConstructFKChecksItem(semiJoin, &memo.FKChecksItemPrivate{
		OriginTable:     origTabMeta.MetaID,
		ReferencedTable: mb.tabID,
		FKOutbound:      false,
		FKOrdinal:       h.fkOrdinal,
		KeyCols:         deleteCols,
		OpName:          mb.opName,
	})

	mb.checks = append(mb.checks, check)
}

// getIndexLaxKeyOrdinals returns the ordinals of all lax key columns in the
// given index. A column's ordinal is the ordered position of that column in the
// owning table.
func getIndexLaxKeyOrdinals(index cat.Index) util.FastIntSet {
	var keyOrds util.FastIntSet
	for i, n := 0, index.LaxKeyColumnCount(); i < n; i++ {
		keyOrds.Add(index.Column(i).Ordinal)
	}
	return keyOrds
}

// findNotNullIndexCol finds the first not-null column in the given index and
// returns its ordinal position in the owner table. There must always be such a
// column, even if it turns out to be an implicit primary key column.
func findNotNullIndexCol(index cat.Index) int {
	for i, n := 0, index.KeyColumnCount(); i < n; i++ {
		indexCol := index.Column(i)
		if !indexCol.IsNullable() {
			return indexCol.Ordinal
		}
	}
	panic(errors.AssertionFailedf("should have found not null column in index"))
}

// resultsNeeded determines whether a statement that might have a RETURNING
// clause needs to provide values for result rows for a downstream plan.
func resultsNeeded(r tree.ReturningClause) bool {
	switch t := r.(type) {
	case *tree.ReturningExprs:
		return true
	case *tree.ReturningNothing, *tree.NoReturningClause:
		return false
	default:
		panic(errors.AssertionFailedf("unexpected ReturningClause type: %T", t))
	}
}

// checkDatumTypeFitsColumnType verifies that a given scalar value type is valid
// to be stored in a column of the given column type.
//
// For the purpose of this analysis, column type aliases are not considered to
// be different (eg. TEXT and VARCHAR will fit the same scalar type String).
//
// This is used by the UPDATE, INSERT and UPSERT code.
func checkDatumTypeFitsColumnType(col cat.Column, typ *types.T) {
	if typ.Equivalent(col.DatumType()) {
		return
	}

	colName := string(col.ColName())
	panic(pgerror.Newf(pgcode.DatatypeMismatch,
		"value type %s doesn't match type %s of column %q",
		typ, col.DatumType(), tree.ErrNameString(colName)))
}

// outboundFKColsUpdated returns true if any of the FK columns for an outbound
// constraint are being updated (according to updateOrds).
func (mb *mutationBuilder) outboundFKColsUpdated(fkOrdinal int) bool {
	fk := mb.tab.OutboundForeignKey(fkOrdinal)
	for i, n := 0, fk.ColumnCount(); i < n; i++ {
		if ord := fk.OriginColumnOrdinal(mb.tab, i); mb.updateOrds[ord] != -1 {
			return true
		}
	}
	return false
}

// inboundFKColsUpdated returns true if any of the FK columns for an inbound
// constraint are being updated (according to updateOrds).
func (mb *mutationBuilder) inboundFKColsUpdated(fkOrdinal int) bool {
	fk := mb.tab.InboundForeignKey(fkOrdinal)
	for i, n := 0, fk.ColumnCount(); i < n; i++ {
		if ord := fk.ReferencedColumnOrdinal(mb.tab, i); mb.updateOrds[ord] != -1 {
			return true
		}
	}
	return false
}

// fkCheckHelper is a type associated with a single FK constraint and is used to
// build the "leaves" of a FK check expression, namely the WithScan of the
// mutation input and the Scan of the other table.
type fkCheckHelper struct {
	mb *mutationBuilder

	fk         cat.ForeignKeyConstraint
	fkOrdinal  int
	fkOutbound bool

	otherTab cat.Table

	// tabOrdinals are the table ordinals of the FK columns in the table that is
	// being mutated. They correspond 1-to-1 to the columns in the
	// ForeignKeyConstraint.
	tabOrdinals []int
	// otherTabOrdinals are the table ordinals of the FK columns in the "other"
	// table. They correspond 1-to-1 to the columns in the ForeignKeyConstraint.
	otherTabOrdinals []int
}

// initWithOutboundFK initializes the helper with an outbound FK constraint.
func (h *fkCheckHelper) initWithOutboundFK(mb *mutationBuilder, fkOrdinal int) bool {
	*h = fkCheckHelper{
		mb:         mb,
		fk:         mb.tab.OutboundForeignKey(fkOrdinal),
		fkOrdinal:  fkOrdinal,
		fkOutbound: true,
	}

	refID := h.fk.ReferencedTableID()
	ref, isAdding, err := mb.b.catalog.ResolveDataSourceByID(mb.b.ctx, cat.Flags{}, refID)
	if err != nil {
		if isAdding {
			// The other table is in the process of being added; ignore the FK relation.
			return false
		}
		panic(err)
	}
	// We need SELECT privileges on the referenced table.
	mb.b.checkPrivilege(opt.DepByID(refID), ref, privilege.SELECT)
	h.otherTab = ref.(cat.Table)

	numCols := h.fk.ColumnCount()
	h.allocOrdinals(numCols)
	for i := 0; i < numCols; i++ {
		h.tabOrdinals[i] = h.fk.OriginColumnOrdinal(mb.tab, i)
		h.otherTabOrdinals[i] = h.fk.ReferencedColumnOrdinal(h.otherTab, i)
	}
	return true
}

// initWithInboundFK initializes the helper with an inbound FK constraint.
//
// Returns false if the FK relation should be ignored (because the other table
// is in the process of being created).
func (h *fkCheckHelper) initWithInboundFK(mb *mutationBuilder, fkOrdinal int) (ok bool) {
	*h = fkCheckHelper{
		mb:         mb,
		fk:         mb.tab.InboundForeignKey(fkOrdinal),
		fkOrdinal:  fkOrdinal,
		fkOutbound: false,
	}

	originID := h.fk.OriginTableID()
	ref, isAdding, err := mb.b.catalog.ResolveDataSourceByID(mb.b.ctx, cat.Flags{}, originID)
	if err != nil {
		if isAdding {
			// The other table is in the process of being added; ignore the FK relation.
			return false
		}
		panic(err)
	}
	// We need SELECT privileges on the origin table.
	mb.b.checkPrivilege(opt.DepByID(originID), ref, privilege.SELECT)
	h.otherTab = ref.(cat.Table)

	numCols := h.fk.ColumnCount()
	h.allocOrdinals(numCols)
	for i := 0; i < numCols; i++ {
		h.tabOrdinals[i] = h.fk.ReferencedColumnOrdinal(mb.tab, i)
		h.otherTabOrdinals[i] = h.fk.OriginColumnOrdinal(h.otherTab, i)
	}

	return true
}

type fkInputScanType uint8

const (
	fkInputScanNewVals fkInputScanType = iota
	fkInputScanFetchedVals
)

// makeFKInputScan constructs a WithScan that iterates over the input to the
// mutation operator. Used in expressions that generate rows for checking for FK
// violations.
//
// The WithScan expression will scan either the new values or the fetched values
// for the given table ordinals (which correspond to FK columns).
//
// Returns the output columns from the WithScan, which map 1-to-1 to
// h.tabOrdinals. Also returns the subset of these columns that can be assumed
// to be not null (either because they are not null in the mutation input or
// because they are non-nullable table columns).
//
func (h *fkCheckHelper) makeFKInputScan(
	typ fkInputScanType,
) (scan memo.RelExpr, outCols opt.ColList, notNullOutCols opt.ColSet) {
	mb := h.mb
	// inputCols are the column IDs from the mutation input that we are scanning.
	inputCols := make(opt.ColList, len(h.tabOrdinals))
	// outCols will store the newly synthesized output columns for WithScan.
	outCols = make(opt.ColList, len(inputCols))
	for i, tabOrd := range h.tabOrdinals {
		if typ == fkInputScanNewVals {
			inputCols[i] = mb.scopeOrdToColID(mb.mapToReturnScopeOrd(tabOrd))
		} else {
			inputCols[i] = mb.scopeOrdToColID(mb.fetchOrds[tabOrd])
		}
		if inputCols[i] == 0 {
			panic(errors.AssertionFailedf("no value for FK column (tabOrd=%d)", tabOrd))
		}

		// Synthesize new column.
		c := mb.b.factory.Metadata().ColumnMeta(inputCols[i])
		outCols[i] = mb.md.AddColumn(c.Alias, c.Type)

		// If a table column is not nullable, NULLs cannot be inserted (the
		// mutation will fail). So for the purposes of FK checks, we can treat
		// these columns as not null.
		if mb.outScope.expr.Relational().NotNullCols.Contains(inputCols[i]) ||
			!mb.tab.Column(tabOrd).IsNullable() {
			notNullOutCols.Add(outCols[i])
		}
	}

	scan = mb.b.factory.ConstructWithScan(&memo.WithScanPrivate{
		With:         mb.withID,
		InCols:       inputCols,
		OutCols:      outCols,
		BindingProps: mb.outScope.expr.Relational(),
		ID:           mb.b.factory.Metadata().NextUniqueID(),
	})
	return scan, outCols, notNullOutCols
}

// buildOtherTableScan builds a Scan of the "other" table.
func (h *fkCheckHelper) buildOtherTableScan() (outScope *scope, tabMeta *opt.TableMeta) {
	otherTabMeta := h.mb.b.addTable(h.otherTab, tree.NewUnqualifiedTableName(h.otherTab.Name()))
	return h.mb.b.buildScan(
		otherTabMeta,
		h.otherTabOrdinals,
		&tree.IndexFlags{IgnoreForeignKeys: true},
		noRowLocking,
		includeMutations,
		h.mb.b.allocScope(),
	), otherTabMeta
}

func (h *fkCheckHelper) allocOrdinals(numCols int) {
	buf := make([]int, numCols*2)
	h.tabOrdinals = buf[:numCols]
	h.otherTabOrdinals = buf[numCols:]
}
