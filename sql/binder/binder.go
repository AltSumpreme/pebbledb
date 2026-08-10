package binder

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/sql/ast"
	"pebbledb/types"
)

type Context struct {
	DatabaseID catalog.DescriptorID
	SchemaID   catalog.DescriptorID
}

type Binder struct {
	catalog *catalog.Catalog
	context Context
}

func New(catalogValue *catalog.Catalog, context Context) (*Binder, error) {
	if catalogValue == nil || context.DatabaseID == 0 || context.SchemaID == 0 {
		return nil, fmt.Errorf("sql binder: catalog, database, and schema are required")
	}
	return &Binder{catalog: catalogValue, context: context}, nil
}

func (binder *Binder) Bind(statement ast.Statement) (Statement, error) {
	switch value := statement.(type) {
	case ast.CreateDatabase:
		return CreateDatabase{Name: value.Name}, nil
	case ast.CreateSchema:
		return binder.bindCreateSchema(value)
	case ast.CreateTable:
		return binder.bindCreateTable(value)
	case ast.CreateIndex:
		return binder.bindCreateIndex(value)
	case ast.DropTable:
		return binder.bindDropTable(value)
	case ast.Insert:
		return binder.bindInsert(value)
	case ast.Select:
		return binder.bindSelect(value)
	case ast.Update:
		return binder.bindUpdate(value)
	case ast.Delete:
		return binder.bindDelete(value)
	case ast.ShowTables:
		return ShowTables{SchemaID: binder.context.SchemaID}, nil
	case ast.DescribeTable:
		table, err := binder.resolveTable(value.Name)
		return DescribeTable{Table: table}, err
	case ast.Begin:
		return Begin{}, nil
	case ast.Commit:
		return Commit{}, nil
	case ast.Rollback:
		return Rollback{}, nil
	default:
		return nil, fmt.Errorf("sql binder: unsupported statement %T", statement)
	}
}

func (binder *Binder) bindCreateSchema(statement ast.CreateSchema) (Statement, error) {
	if len(statement.Name.Parts) == 1 {
		return CreateSchema{DatabaseID: binder.context.DatabaseID, Name: statement.Name.Parts[0]}, nil
	}
	if len(statement.Name.Parts) == 2 {
		database, err := binder.catalog.GetDatabase(statement.Name.Parts[0])
		if err != nil {
			return nil, err
		}
		return CreateSchema{DatabaseID: database.ID, Name: statement.Name.Parts[1]}, nil
	}
	return nil, fmt.Errorf("sql binder: schema name must have one or two parts")
}

func (binder *Binder) bindCreateTable(statement ast.CreateTable) (Statement, error) {
	schemaID, tableName, err := binder.resolveTargetSchema(statement.Name)
	if err != nil {
		return nil, err
	}
	result := CreateTable{SchemaID: schemaID, Name: tableName}
	columnByName := make(map[string]codec.ColumnDescriptor, len(statement.Columns))
	for index, definition := range statement.Columns {
		if _, exists := columnByName[definition.Name]; exists {
			return nil, fmt.Errorf("sql binder: duplicate column %q", definition.Name)
		}
		typeValue, err := types.ParseType(definition.TypeName)
		if err != nil {
			return nil, fmt.Errorf("sql binder: column %q: %w", definition.Name, err)
		}
		column := codec.ColumnDescriptor{ID: uint32(index + 1), Name: definition.Name, Type: typeValue, Nullable: definition.Nullable}
		result.Columns = append(result.Columns, column)
		columnByName[definition.Name] = column
		if definition.Unique {
			result.UniqueColumn = append(result.UniqueColumn, column.ID)
		}
	}
	seenPrimary := make(map[uint32]struct{}, len(statement.PrimaryKey))
	for _, name := range statement.PrimaryKey {
		column, exists := columnByName[name]
		if !exists {
			return nil, fmt.Errorf("sql binder: primary key references unknown column %q", name)
		}
		if _, exists := seenPrimary[column.ID]; exists {
			return nil, fmt.Errorf("sql binder: duplicate primary-key column %q", name)
		}
		if column.Nullable {
			for index := range result.Columns {
				if result.Columns[index].ID == column.ID {
					result.Columns[index].Nullable = false
				}
			}
		}
		seenPrimary[column.ID] = struct{}{}
		result.PrimaryKey = append(result.PrimaryKey, column.ID)
	}
	return result, nil
}

func (binder *Binder) bindCreateIndex(statement ast.CreateIndex) (Statement, error) {
	table, err := binder.resolveTable(statement.Table)
	if err != nil {
		return nil, err
	}
	columnIDs, err := resolveColumnNames(table, statement.Columns)
	if err != nil {
		return nil, err
	}
	return CreateIndex{Table: table, Name: statement.Name, ColumnIDs: columnIDs, Unique: statement.Unique}, nil
}

func (binder *Binder) bindDropTable(statement ast.DropTable) (Statement, error) {
	table, err := binder.resolveTable(statement.Name)
	if err != nil {
		if statement.IfExists && errors.Is(err, catalog.ErrNotFound) {
			return DropTable{Missing: statement.Name.String(), IfExists: true}, nil
		}
		return nil, err
	}
	return DropTable{Table: table, IfExists: statement.IfExists}, nil
}

func (binder *Binder) bindInsert(statement ast.Insert) (Statement, error) {
	table, err := binder.resolveTable(statement.Table)
	if err != nil {
		return nil, err
	}
	columns := table.Schema.Columns
	if len(statement.Columns) > 0 {
		columns = nil
		seen := make(map[uint32]struct{}, len(statement.Columns))
		for _, name := range statement.Columns {
			column, err := findColumn(table, name)
			if err != nil {
				return nil, err
			}
			if _, exists := seen[column.ID]; exists {
				return nil, fmt.Errorf("sql binder: column %q specified more than once", name)
			}
			seen[column.ID] = struct{}{}
			columns = append(columns, column)
		}
	}
	result := Insert{Table: table}
	for rowIndex, expressions := range statement.Rows {
		if len(expressions) != len(columns) {
			return nil, fmt.Errorf("sql binder: row %d has %d values for %d target columns", rowIndex+1, len(expressions), len(columns))
		}
		row := codec.Row{SchemaVersion: table.Schema.Version, Values: make(map[uint32]types.Value, len(table.Schema.Columns))}
		provided := make(map[uint32]struct{}, len(columns))
		for index, expression := range expressions {
			value, err := coerceLiteral(expression, columns[index].Type, columns[index].Nullable)
			if err != nil {
				return nil, fmt.Errorf("sql binder: row %d column %q: %w", rowIndex+1, columns[index].Name, err)
			}
			row.Values[columns[index].ID] = value
			provided[columns[index].ID] = struct{}{}
		}
		for _, column := range table.Schema.Columns {
			if _, exists := provided[column.ID]; exists {
				continue
			}
			if !column.Nullable {
				return nil, fmt.Errorf("sql binder: missing value for non-null column %q", column.Name)
			}
			null, _ := types.NullValue(column.Type)
			row.Values[column.ID] = null
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func (binder *Binder) bindSelect(statement ast.Select) (Statement, error) {
	table, err := binder.resolveTable(statement.From)
	if err != nil {
		return nil, err
	}
	result := Select{Table: table, Limit: statement.Limit}
	scope := []catalog.TableDescriptor{table}
	for _, rawJoin := range statement.Joins {
		joinedTable, err := binder.resolveTable(rawJoin.Table)
		if err != nil {
			return nil, err
		}
		for _, existing := range scope {
			if existing.ID == joinedTable.ID {
				return nil, fmt.Errorf("sql binder: table %q occurs more than once without aliases", joinedTable.Name)
			}
		}
		scope = append(scope, joinedTable)
		on, err := binder.bindExpression(rawJoin.On, scope)
		if err != nil {
			return nil, err
		}
		if on.Type != types.BoolType() {
			return nil, fmt.Errorf("sql binder: JOIN ON expression must be BOOL")
		}
		result.Joins = append(result.Joins, Join{Table: joinedTable, On: on})
	}
	for _, item := range statement.Items {
		if _, star := item.Expression.(ast.Star); star {
			for _, scopedTable := range scope {
				for _, column := range scopedTable.Schema.Columns {
					result.Projection = append(result.Projection, Projection{Expression: boundColumn(scopedTable.ID, column), Alias: column.Name})
				}
			}
			continue
		}
		expression, err := binder.bindExpression(item.Expression, scope)
		if err != nil {
			return nil, err
		}
		result.Projection = append(result.Projection, Projection{Expression: expression, Alias: item.Alias})
	}
	if statement.Where != nil {
		result.Filter, err = binder.bindExpression(statement.Where, scope)
		if err != nil {
			return nil, err
		}
		if result.Filter.Type != types.BoolType() {
			return nil, fmt.Errorf("sql binder: WHERE expression must be BOOL, got %s", result.Filter.Type)
		}
	}
	for _, ordering := range statement.OrderBy {
		expression, err := binder.bindExpression(ordering.Expression, scope)
		if err != nil {
			return nil, err
		}
		result.Ordering = append(result.Ordering, Ordering{Expression: expression, Descending: ordering.Descending})
	}
	return result, nil
}

func (binder *Binder) bindUpdate(statement ast.Update) (Statement, error) {
	table, err := binder.resolveTable(statement.Table)
	if err != nil {
		return nil, err
	}
	result := Update{Table: table}
	seen := make(map[uint32]struct{}, len(statement.Assignments))
	for _, assignment := range statement.Assignments {
		column, err := findColumn(table, assignment.Column)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[column.ID]; exists {
			return nil, fmt.Errorf("sql binder: column %q assigned more than once", column.Name)
		}
		var expression *Expression
		if isLiteralSyntax(assignment.Value) {
			literal, err := coerceLiteral(assignment.Value, column.Type, column.Nullable)
			if err != nil {
				return nil, fmt.Errorf("sql binder: assignment to %q: %w", column.Name, err)
			}
			expression = boundLiteral(literal)
		} else {
			expression, err = binder.bindExpression(assignment.Value, []catalog.TableDescriptor{table})
			if err != nil {
				return nil, err
			}
		}
		if !assignmentCompatible(column.Type, expression.Type) {
			return nil, fmt.Errorf("sql binder: cannot assign %s to %s column %q", expression.Type, column.Type, column.Name)
		}
		result.Assignments = append(result.Assignments, Assignment{ColumnID: column.ID, Value: expression})
		seen[column.ID] = struct{}{}
	}
	if statement.Where != nil {
		result.Filter, err = binder.bindExpression(statement.Where, []catalog.TableDescriptor{table})
		if err != nil || result.Filter.Type != types.BoolType() {
			if err == nil {
				err = fmt.Errorf("sql binder: WHERE expression must be BOOL")
			}
			return nil, err
		}
	}
	return result, nil
}

func (binder *Binder) bindDelete(statement ast.Delete) (Statement, error) {
	table, err := binder.resolveTable(statement.Table)
	if err != nil {
		return nil, err
	}
	result := Delete{Table: table}
	if statement.Where != nil {
		result.Filter, err = binder.bindExpression(statement.Where, []catalog.TableDescriptor{table})
		if err != nil || result.Filter.Type != types.BoolType() {
			if err == nil {
				err = fmt.Errorf("sql binder: WHERE expression must be BOOL")
			}
			return nil, err
		}
	}
	return result, nil
}

func (binder *Binder) bindExpression(expression ast.Expression, scope []catalog.TableDescriptor) (*Expression, error) {
	switch value := expression.(type) {
	case ast.ColumnReference:
		table, column, err := findColumnInScope(scope, value.Name)
		if err != nil {
			return nil, err
		}
		return boundColumn(table.ID, column), nil
	case ast.Literal:
		return bindUntypedLiteral(value)
	case ast.UnaryExpression:
		argument, err := binder.bindExpression(value.Value, scope)
		if err != nil {
			return nil, err
		}
		operator := strings.ToUpper(value.Operator)
		if operator == "NOT" && argument.Type != types.BoolType() {
			return nil, fmt.Errorf("sql binder: NOT requires BOOL")
		}
		if (operator == "+" || operator == "-") && !isNumeric(argument.Type) {
			return nil, fmt.Errorf("sql binder: unary %s requires a numeric value", operator)
		}
		return &Expression{Kind: UnaryExpression, Type: argument.Type, Nullable: argument.Nullable, Operator: operator, Arguments: []*Expression{argument}}, nil
	case ast.BinaryExpression:
		left, err := binder.bindExpression(value.Left, scope)
		if err != nil {
			return nil, err
		}
		right, err := binder.bindExpression(value.Right, scope)
		if err != nil {
			return nil, err
		}
		if left.Type.Valid() && isLiteralSyntax(value.Right) {
			literal, coercionErr := coerceLiteral(value.Right, left.Type, true)
			if coercionErr == nil {
				right = boundLiteral(literal)
			}
		}
		if right.Type.Valid() && isLiteralSyntax(value.Left) {
			literal, coercionErr := coerceLiteral(value.Left, right.Type, true)
			if coercionErr == nil {
				left = boundLiteral(literal)
			}
		}
		return bindBinary(value.Operator, left, right)
	case ast.FunctionCall:
		return binder.bindFunction(value, scope)
	case ast.Star:
		return nil, fmt.Errorf("sql binder: star is only valid as a projection or COUNT argument")
	default:
		return nil, fmt.Errorf("sql binder: unsupported expression %T", expression)
	}
}

func (binder *Binder) bindFunction(call ast.FunctionCall, scope []catalog.TableDescriptor) (*Expression, error) {
	name := strings.ToLower(call.Name)
	if name == "count" && len(call.Arguments) == 1 {
		if _, star := call.Arguments[0].(ast.Star); star {
			return &Expression{Kind: FunctionExpression, Type: types.BigIntType(), Name: name}, nil
		}
	}
	arguments := make([]*Expression, 0, len(call.Arguments))
	for _, raw := range call.Arguments {
		argument, err := binder.bindExpression(raw, scope)
		if err != nil {
			return nil, err
		}
		arguments = append(arguments, argument)
	}
	switch name {
	case "count":
		if len(arguments) != 1 {
			return nil, fmt.Errorf("sql binder: COUNT requires one argument")
		}
		return &Expression{Kind: FunctionExpression, Type: types.BigIntType(), Name: name, Arguments: arguments}, nil
	case "sum":
		if len(arguments) != 1 || !isNumeric(arguments[0].Type) {
			return nil, fmt.Errorf("sql binder: SUM requires one numeric argument")
		}
		return &Expression{Kind: FunctionExpression, Type: arguments[0].Type, Nullable: true, Name: name, Arguments: arguments}, nil
	case "lower", "upper":
		if len(arguments) != 1 || arguments[0].Type != types.TextType() {
			return nil, fmt.Errorf("sql binder: %s requires one TEXT argument", strings.ToUpper(name))
		}
		return &Expression{Kind: FunctionExpression, Type: types.TextType(), Nullable: arguments[0].Nullable, Name: name, Arguments: arguments}, nil
	default:
		return nil, fmt.Errorf("sql binder: unknown function %q", call.Name)
	}
}

func bindBinary(operator string, left, right *Expression) (*Expression, error) {
	operator = strings.ToUpper(operator)
	if operator == "AND" || operator == "OR" {
		if left.Type != types.BoolType() || right.Type != types.BoolType() {
			return nil, fmt.Errorf("sql binder: %s requires BOOL operands", operator)
		}
		return &Expression{Kind: BinaryExpression, Type: types.BoolType(), Nullable: left.Nullable || right.Nullable, Operator: operator, Arguments: []*Expression{left, right}}, nil
	}
	if strings.Contains(" = != <> < <= > >= ", " "+operator+" ") {
		if !comparableTypes(left.Type, right.Type) {
			return nil, fmt.Errorf("sql binder: cannot compare %s and %s", left.Type, right.Type)
		}
		return &Expression{Kind: BinaryExpression, Type: types.BoolType(), Nullable: left.Nullable || right.Nullable, Operator: operator, Arguments: []*Expression{left, right}}, nil
	}
	if strings.Contains(" + - * / % ", " "+operator+" ") {
		resultType, ok := numericResultType(left.Type, right.Type)
		if !ok {
			return nil, fmt.Errorf("sql binder: %s requires compatible numeric operands", operator)
		}
		return &Expression{Kind: BinaryExpression, Type: resultType, Nullable: left.Nullable || right.Nullable, Operator: operator, Arguments: []*Expression{left, right}}, nil
	}
	return nil, fmt.Errorf("sql binder: unknown binary operator %q", operator)
}

func bindUntypedLiteral(literal ast.Literal) (*Expression, error) {
	switch literal.Kind {
	case ast.NullLiteral:
		null, _ := types.NullValue(types.TextType())
		return &Expression{Kind: LiteralExpression, Type: types.TextType(), Nullable: true, Literal: null}, nil
	case ast.BoolLiteral:
		return &Expression{Kind: LiteralExpression, Type: types.BoolType(), Literal: types.BoolValue(strings.EqualFold(literal.Raw, "true"))}, nil
	case ast.StringLiteral:
		value, err := types.TextValue(literal.Raw)
		return &Expression{Kind: LiteralExpression, Type: types.TextType(), Literal: value}, err
	case ast.NumberLiteral:
		if strings.Contains(literal.Raw, ".") {
			parts := strings.Split(literal.Raw, ".")
			precision := len(strings.TrimLeft(parts[0], "0")) + len(parts[1])
			if precision < 1 {
				precision = 1
			}
			typeValue, err := types.DecimalType(precision, len(parts[1]))
			if err != nil {
				return nil, err
			}
			value, err := types.DecimalValue(typeValue, literal.Raw)
			return &Expression{Kind: LiteralExpression, Type: typeValue, Literal: value}, err
		}
		integer, err := strconv.ParseInt(literal.Raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("sql binder: invalid integer literal %q", literal.Raw)
		}
		if integer >= math.MinInt32 && integer <= math.MaxInt32 {
			return &Expression{Kind: LiteralExpression, Type: types.IntType(), Literal: types.IntValue(int32(integer))}, nil
		}
		return &Expression{Kind: LiteralExpression, Type: types.BigIntType(), Literal: types.BigIntValue(integer)}, nil
	default:
		return nil, fmt.Errorf("sql binder: unsupported literal")
	}
}

func coerceLiteral(expression ast.Expression, target types.Type, nullable bool) (types.Value, error) {
	sign := ""
	if unary, ok := expression.(ast.UnaryExpression); ok && (unary.Operator == "+" || unary.Operator == "-") {
		sign = unary.Operator
		expression = unary.Value
	}
	literal, ok := expression.(ast.Literal)
	if !ok {
		return types.Value{}, fmt.Errorf("INSERT values must currently be literals")
	}
	if literal.Kind == ast.NullLiteral {
		if !nullable {
			return types.Value{}, fmt.Errorf("NULL violates NOT NULL")
		}
		return types.NullValue(target)
	}
	switch target.Kind() {
	case types.BoolKind:
		if literal.Kind != ast.BoolLiteral {
			return types.Value{}, fmt.Errorf("expected BOOL literal")
		}
		return types.BoolValue(strings.EqualFold(literal.Raw, "true")), nil
	case types.TextKind:
		if literal.Kind != ast.StringLiteral {
			return types.Value{}, fmt.Errorf("expected string literal")
		}
		return types.TextValue(literal.Raw)
	case types.IntKind:
		if literal.Kind != ast.NumberLiteral || strings.Contains(literal.Raw, ".") {
			return types.Value{}, fmt.Errorf("expected INT literal")
		}
		value, err := strconv.ParseInt(sign+literal.Raw, 10, 32)
		if err != nil {
			return types.Value{}, fmt.Errorf("invalid INT literal: %w", err)
		}
		return types.IntValue(int32(value)), nil
	case types.BigIntKind:
		if literal.Kind != ast.NumberLiteral || strings.Contains(literal.Raw, ".") {
			return types.Value{}, fmt.Errorf("expected BIGINT literal")
		}
		value, err := strconv.ParseInt(sign+literal.Raw, 10, 64)
		if err != nil {
			return types.Value{}, fmt.Errorf("invalid BIGINT literal: %w", err)
		}
		return types.BigIntValue(value), nil
	case types.DecimalKind:
		if literal.Kind != ast.NumberLiteral {
			return types.Value{}, fmt.Errorf("expected DECIMAL literal")
		}
		return types.DecimalValue(target, sign+literal.Raw)
	default:
		return types.Value{}, fmt.Errorf("unsupported target type %s", target)
	}
}

func (binder *Binder) resolveTable(name ast.Name) (catalog.TableDescriptor, error) {
	schemaID, tableName, err := binder.resolveTargetSchema(name)
	if err != nil {
		return catalog.TableDescriptor{}, err
	}
	return binder.catalog.GetTable(schemaID, tableName)
}

func (binder *Binder) resolveTargetSchema(name ast.Name) (catalog.DescriptorID, string, error) {
	switch len(name.Parts) {
	case 1:
		return binder.context.SchemaID, name.Parts[0], nil
	case 2:
		schema, err := binder.catalog.GetSchema(binder.context.DatabaseID, name.Parts[0])
		return schema.ID, name.Parts[1], err
	case 3:
		database, err := binder.catalog.GetDatabase(name.Parts[0])
		if err != nil {
			return 0, "", err
		}
		schema, err := binder.catalog.GetSchema(database.ID, name.Parts[1])
		return schema.ID, name.Parts[2], err
	default:
		return 0, "", fmt.Errorf("sql binder: invalid qualified name %q", name.String())
	}
}

func findColumn(table catalog.TableDescriptor, name string) (codec.ColumnDescriptor, error) {
	for _, column := range table.Schema.Columns {
		if column.Name == name {
			return column, nil
		}
	}
	return codec.ColumnDescriptor{}, fmt.Errorf("sql binder: column %q does not exist in table %q", name, table.Name)
}

func resolveColumnNames(table catalog.TableDescriptor, names []string) ([]uint32, error) {
	seen := make(map[uint32]struct{}, len(names))
	result := make([]uint32, 0, len(names))
	for _, name := range names {
		column, err := findColumn(table, name)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[column.ID]; exists {
			return nil, fmt.Errorf("sql binder: column %q specified more than once", name)
		}
		seen[column.ID] = struct{}{}
		result = append(result, column.ID)
	}
	return result, nil
}

func findColumnInScope(scope []catalog.TableDescriptor, name ast.Name) (catalog.TableDescriptor, codec.ColumnDescriptor, error) {
	columnName := name.Parts[len(name.Parts)-1]
	qualifier := ""
	if len(name.Parts) > 1 {
		qualifier = name.Parts[len(name.Parts)-2]
	}
	var matchedTable catalog.TableDescriptor
	var matchedColumn codec.ColumnDescriptor
	matches := 0
	for _, table := range scope {
		if qualifier != "" && qualifier != table.Name {
			continue
		}
		column, err := findColumn(table, columnName)
		if err != nil {
			continue
		}
		matchedTable, matchedColumn = table, column
		matches++
	}
	if matches == 0 {
		return catalog.TableDescriptor{}, codec.ColumnDescriptor{}, fmt.Errorf("sql binder: column %q does not exist in query scope", name.String())
	}
	if matches > 1 {
		return catalog.TableDescriptor{}, codec.ColumnDescriptor{}, fmt.Errorf("sql binder: column %q is ambiguous", columnName)
	}
	return matchedTable, matchedColumn, nil
}

func boundColumn(tableID catalog.DescriptorID, column codec.ColumnDescriptor) *Expression {
	return &Expression{Kind: ColumnExpression, Type: column.Type, Nullable: column.Nullable, TableID: tableID, ColumnID: column.ID, Name: column.Name}
}

func boundLiteral(value types.Value) *Expression {
	return &Expression{Kind: LiteralExpression, Type: value.Type(), Nullable: value.IsNull(), Literal: value}
}

func isLiteralSyntax(expression ast.Expression) bool {
	if _, ok := expression.(ast.Literal); ok {
		return true
	}
	if unary, ok := expression.(ast.UnaryExpression); ok && (unary.Operator == "+" || unary.Operator == "-") {
		_, ok = unary.Value.(ast.Literal)
		return ok
	}
	return false
}

func assignmentCompatible(target, source types.Type) bool {
	return target == source || (target.Kind() == types.BigIntKind && source.Kind() == types.IntKind)
}

func comparableTypes(left, right types.Type) bool {
	if !left.Valid() || !right.Valid() {
		return true // an untyped NULL adopts the other operand's type
	}
	return left == right || (isInteger(left) && isInteger(right))
}

func numericResultType(left, right types.Type) (types.Type, bool) {
	if !isNumeric(left) || !isNumeric(right) {
		return types.Type{}, false
	}
	if left.Kind() == types.DecimalKind || right.Kind() == types.DecimalKind {
		if left == right {
			return left, true
		}
		return types.Type{}, false
	}
	if left.Kind() == types.BigIntKind || right.Kind() == types.BigIntKind {
		return types.BigIntType(), true
	}
	return types.IntType(), true
}

func isInteger(typeValue types.Type) bool {
	return typeValue.Kind() == types.IntKind || typeValue.Kind() == types.BigIntKind
}

func isNumeric(typeValue types.Type) bool {
	return isInteger(typeValue) || typeValue.Kind() == types.DecimalKind
}
