package drift

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/dollarshaveclub/amino/src/jobmanager"
)

// RawDynamoItem models an item from DynamoDB as returned by the API
type RawDynamoItem map[string]*dynamodb.AttributeValue

// DynamoMigrationFunction is a callback run for each item in the DynamoDB table
// item is the raw item
// action is the DrifterAction object used to mutate/add/remove items
type DynamoMigrationFunction func(item RawDynamoItem, action *DrifterAction) error

// DynamoDrifterMigration models an individual migration
type DynamoDrifterMigration struct {
	Number      uint                    `dynamodb:"Number"`      // Monotonic number of the migration (ascending)
	TableName   string                  `dynamodb:"TableName"`   // DynamoDB table the migration applies to
	Description string                  `dynamodb:"Description"` // Free-form description of what the migration does
	Callback    DynamoMigrationFunction `dynamodb:"-"`           // Callback for each item in the table
}

// DynamoDrifter is the object that manages and performs migrations
type DynamoDrifter struct {
	MetaTableName string             // Table to store migration tracking metadata
	DynamoDB      *dynamodb.DynamoDB // Fully initialized and authenticated DynamoDB client
	q             actionQueue
}

func (dd *DynamoDrifter) createMetaTable(pwrite, pread uint, metatable string) error {
	cti := &dynamodb.CreateTableInput{
		TableName: aws.String(metatable),
		AttributeDefinitions: []*dynamodb.AttributeDefinition{
			&dynamodb.AttributeDefinition{
				AttributeName: aws.String("Number"),
				AttributeType: aws.String("N"),
			},
			&dynamodb.AttributeDefinition{
				AttributeName: aws.String("TableName"),
				AttributeType: aws.String("S"),
			},
			&dynamodb.AttributeDefinition{
				AttributeName: aws.String("Description"),
				AttributeType: aws.String("S"),
			},
		},
		KeySchema: []*dynamodb.KeySchemaElement{
			&dynamodb.KeySchemaElement{
				AttributeName: aws.String("Number"),
				KeyType:       aws.String("HASH"),
			},
		},
		ProvisionedThroughput: &dynamodb.ProvisionedThroughput{
			ReadCapacityUnits:  aws.Int64(int64(pread)),
			WriteCapacityUnits: aws.Int64(int64(pwrite)),
		},
	}
	_, err := dd.DynamoDB.CreateTable(cti)
	return err
}

func (dd *DynamoDrifter) findTable(table string) (bool, error) {
	var err error
	var lto *dynamodb.ListTablesOutput
	lti := &dynamodb.ListTablesInput{}
	for {
		lto, err = dd.DynamoDB.ListTables(lti)
		if err != nil {
			return false, fmt.Errorf("error listing tables: %v", err)
		}
		for _, tn := range lto.TableNames {
			if tn != nil && *tn == table {
				return true, nil
			}
		}
		if lto.LastEvaluatedTableName == nil {
			return false, nil
		}
		lti.ExclusiveStartTableName = lto.LastEvaluatedTableName
	}
}

// Init creates the metadata table if necessary.
// pread and pwrite are the provisioned read and write values to use with table creation, if necessary
func (dd *DynamoDrifter) Init(pwrite, pread uint) error {
	if dd.DynamoDB == nil {
		return fmt.Errorf("DynamoDB client is required")
	}
	extant, err := dd.findTable(dd.MetaTableName)
	if err != nil {
		return fmt.Errorf("error checking if meta table exists: %v", err)
	}
	if !extant {
		err = dd.createMetaTable(pwrite, pread, dd.MetaTableName)
		if err != nil {
			return fmt.Errorf("error creating meta table: %v", err)
		}
	}
	return nil
}

// Applied returns all applied migrations as tracked in metadata table in ascending order
func (dd *DynamoDrifter) Applied() ([]DynamoDrifterMigration, error) {
	if dd.DynamoDB == nil {
		return nil, fmt.Errorf("DynamoDB client is required")
	}
	in := &dynamodb.ScanInput{
		TableName: &dd.MetaTableName,
	}
	ms := []DynamoDrifterMigration{}
	var consumeErr error
	consumePage := func(resp *dynamodb.ScanOutput, last bool) bool {
		for _, v := range resp.Items {
			m := DynamoDrifterMigration{}
			consumeErr = dynamodbattribute.UnmarshalMap(v, &m)
			if consumeErr != nil {
				return false // stop paging
			}
			ms = append(ms, m)
		}
		return true
	}

	err := dd.DynamoDB.ScanPages(in, consumePage)
	if err != nil {
		return nil, err
	}
	if consumeErr != nil {
		return nil, consumeErr
	}

	// sort by Number
	sort.Slice(ms, func(i, j int) bool { return ms[i].Number < ms[j].Number })

	return ms, nil
}

func (dd *DynamoDrifter) doCallback(ctx context.Context, params ...interface{}) error {
	if len(params) != 3 {
		return fmt.Errorf("bad parameter count: %v (want 3)", len(params))
	}
	callback, ok := params[0].(DynamoMigrationFunction)
	if !ok {
		return fmt.Errorf("bad type for DynamoMigrationFunction: %T", params[0])
	}
	item, ok := params[1].(RawDynamoItem)
	if !ok {
		return fmt.Errorf("bad type for RawDynamoItem: %T", params[1])
	}
	da, ok := params[2].(*DrifterAction)
	if !ok {
		return fmt.Errorf("bad type for *DrifterAction: %T", params[2])
	}
	return callback(item, da)
}

type errorCollector struct {
	sync.Mutex
	errs []error
}

func (ec *errorCollector) clear() {
	ec.Lock()
	ec.errs = []error{}
	ec.Unlock()
}

func (ec *errorCollector) HandleError(err error) error {
	ec.Lock()
	ec.errs = append(ec.errs, err)
	ec.Unlock()
	return nil
}

// runCallbacks gets items from the target table in batches of size concurrency, populates a JobManager with them and then executes all jobs in parallel
func (dd *DynamoDrifter) runCallbacks(ctx context.Context, migration *DynamoDrifterMigration, concurrency uint, failOnFirstError bool) (*DrifterAction, []error) {
	errs := []error{}
	ec := errorCollector{}
	da := &DrifterAction{}
	jm := jobmanager.New()
	jm.ErrorHandler = &ec
	jm.Concurrency = concurrency
	jm.Identifier = "migration-callbacks"

	si := &dynamodb.ScanInput{
		ConsistentRead: aws.Bool(true),
		TableName:      &migration.TableName,
		Limit:          aws.Int64(int64(concurrency)),
	}
	for {
		so, err := dd.DynamoDB.Scan(si)
		if err != nil {
			return nil, []error{fmt.Errorf("error scanning migration table: %v", err)}
		}
		j := &jobmanager.Job{
			Job: dd.doCallback,
		}
		for _, item := range so.Items {
			jm.AddJob(j, migration.Callback, item, da)
		}
		jm.Run(ctx)
		if len(ec.errs) != 0 && failOnFirstError {
			return nil, ec.errs
		}
		errs = append(errs, ec.errs...)
		ec.clear()
		if so.LastEvaluatedKey == nil {
			return da, errs
		}
		si.ExclusiveStartKey = so.LastEvaluatedKey
	}
}

func (dd *DynamoDrifter) doAction(ctx context.Context, params ...interface{}) error {
	return nil
}

func (dd *DynamoDrifter) executeActions(ctx context.Context, da *DrifterAction, concurrency uint) []error {
	ec := errorCollector{}
	jm := jobmanager.New()
	jm.ErrorHandler = &ec
	jm.Concurrency = concurrency
	jm.Identifier = "migration-actions"
	for _, action := range da.aq.q {
		j := &jobmanager.Job{
			Job: dd.doAction,
		}
		jm.AddJob(j, action)
	}
	jm.Run(ctx)
	return ec.errs
}

func (dd *DynamoDrifter) insertMetaItem(m *DynamoDrifterMigration) error {
	mi, err := dynamodbattribute.MarshalMap(m)
	if err != nil {
		return fmt.Errorf("error marshaling migration: %v", err)
	}
	pi := &dynamodb.PutItemInput{
		TableName: &dd.MetaTableName,
		Item:      mi,
	}
	_, err = dd.DynamoDB.PutItem(pi)
	if err != nil {
		return fmt.Errorf("error inserting migration item into meta table: %v", err)
	}
	return nil
}

func (dd *DynamoDrifter) deleteMetaItem(m *DynamoDrifterMigration) error {
	di := &dynamodb.DeleteItemInput{
		Key: map[string]*dynamodb.AttributeValue{
			"Number": &dynamodb.AttributeValue{
				N: aws.String(strconv.Itoa(int(m.Number))),
			},
		},
	}
	_, err := dd.DynamoDB.DeleteItem(di)
	if err != nil {
		return fmt.Errorf("error deleting item from meta table: %v", err)
	}
	return nil
}

func (dd *DynamoDrifter) run(ctx context.Context, migration *DynamoDrifterMigration, concurrency uint, failOnFirstError bool) []error {
	if migration == nil || migration.Callback == nil {
		return []error{fmt.Errorf("migration is required")}
	}
	if concurrency == 0 {
		concurrency = 1
	}
	if migration.TableName == "" {
		return []error{fmt.Errorf("TableName is required")}
	}
	extant, err := dd.findTable(migration.TableName)
	if err != nil {
		return []error{fmt.Errorf("error finding migration table: %v", err)}
	}
	if !extant {
		return []error{fmt.Errorf("table %v not found", migration.TableName)}
	}
	da, errs := dd.runCallbacks(ctx, migration, concurrency, failOnFirstError)
	if len(errs) != 0 {
		return errs
	}
	errs = dd.executeActions(ctx, da, concurrency)
	if len(errs) != 0 {
		return errs
	}
	return []error{}
}

// Run runs an individual migration at the specified concurrency and blocks until finished.
// concurrency controls the number of table items processed concurrently (value of one will guarantee order of migration actions).
// failOnFirstError causes Run to abort on first error, otherwise the errors will be queued and reported only after all items have been processed.
func (dd *DynamoDrifter) Run(ctx context.Context, migration *DynamoDrifterMigration, concurrency uint, failOnFirstError bool) []error {
	if dd.DynamoDB == nil {
		return []error{fmt.Errorf("DynamoDB client is required")}
	}
	errs := dd.run(ctx, migration, concurrency, failOnFirstError)
	if len(errs) != 0 {
		return errs
	}
	err := dd.insertMetaItem(migration)
	if err != nil {
		return []error{err}
	}
	return []error{}
}

// Undo "undoes" a migration by running the supplied migration but deletes the corresponding metadata record if successful
func (dd *DynamoDrifter) Undo(ctx context.Context, undoMigration *DynamoDrifterMigration, concurrency uint, failOnFirstError bool) []error {
	if dd.DynamoDB == nil {
		return []error{fmt.Errorf("DynamoDB client is required")}
	}
	errs := dd.run(ctx, undoMigration, concurrency, failOnFirstError)
	if len(errs) != 0 {
		return errs
	}
	err := dd.deleteMetaItem(undoMigration)
	if err != nil {
		return []error{err}
	}
	return []error{}
}

type actionType int

const (
	updateAction actionType = iota
	insertAction
	deleteAction
)

type action struct {
	atype        actionType
	keys         RawDynamoItem
	values       RawDynamoItem
	item         RawDynamoItem
	updExpr      string
	expAttrNames map[string]*string
	tableName    string
}

type actionQueue struct {
	sync.Mutex
	q []action
}

// DrifterAction is an object useful for performing actions within the migration callback. All actions performed by methods on DrifterAction are queued and performed *after* all existing items have been iterated over and callbacks performed.
// DrifterAction can be used in multiple goroutines by the callback, but must not be retained after the callback returns.
// If concurrency > 1, order of queued operations cannot be guaranteed.
type DrifterAction struct {
	dyn *dynamodb.DynamoDB
	aq  actionQueue
}

// Update mutates the given keys using fields and updateExpression.
// keys and values are arbitrary structs with "dynamodb" annotations. IMPORTANT: annotation names must match the names used in updateExpression.
// updateExpression is the native DynamoDB update expression. Ex: "SET foo = :bar" (in this example keys must have a field annotated "foo" and values must have a field annotated ":bar").
//
// Required: keys, values, updateExpression
//
// Optional: expressionAttributeNames (used if a value name is reserved keyword), tableName (defaults to migration table)
func (da *DrifterAction) Update(keys interface{}, values interface{}, updateExpression string, expressionAttributeNames map[string]string, tableName string) error {
	mkeys, err := dynamodbattribute.MarshalMap(keys)
	if err != nil {
		return fmt.Errorf("error marshaling keys: %v", err)
	}
	mvals, err := dynamodbattribute.MarshalMap(values)
	if err != nil {
		return fmt.Errorf("error marshaling values: %v", err)
	}
	if updateExpression == "" {
		return fmt.Errorf("updateExpression is required")
	}
	var ean map[string]*string
	if expressionAttributeNames != nil {
		for k, v := range expressionAttributeNames {
			ean[k] = &v
		}
	}
	ua := action{
		atype:        updateAction,
		keys:         mkeys,
		values:       mvals,
		updExpr:      updateExpression,
		expAttrNames: ean,
		tableName:    tableName,
	}
	da.aq.Lock()
	da.aq.q = append(da.aq.q, ua)
	da.aq.Unlock()
	return nil
}

// Insert inserts item into the specified table.
// item is an arbitrary struct with "dynamodb" annotations.
// tableName is optional (defaults to migration table).
func (da *DrifterAction) Insert(item interface{}, tableName string) error {
	mitem, err := dynamodbattribute.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("error marshaling item: %v", err)
	}
	ia := action{
		atype:     insertAction,
		item:      mitem,
		tableName: tableName,
	}
	da.aq.Lock()
	da.aq.q = append(da.aq.q, ia)
	da.aq.Unlock()
	return nil
}

// Delete deletes the specified item(s).
// keys is an arbitrary struct with "dynamodb" annotations.
// tableName is optional (defaults to migration table).
func (da *DrifterAction) Delete(keys interface{}, tableName string) error {
	mkeys, err := dynamodbattribute.MarshalMap(keys)
	if err != nil {
		return fmt.Errorf("error marshaling keys: %v", err)
	}
	dla := action{
		atype:     deleteAction,
		keys:      mkeys,
		tableName: tableName,
	}
	da.aq.Lock()
	da.aq.q = append(da.aq.q, dla)
	da.aq.Unlock()
	return nil
}

// DynamoDB returns the DynamoDB client object
func (da *DrifterAction) DynamoDB() *dynamodb.DynamoDB {
	return da.dyn
}
