package caches

import (
	"reflect"
	"regexp"
	"sync"

	"gorm.io/gorm"
)

type Caches struct {
	callbacks map[queryType]func(db *gorm.DB)
	Conf      *Config

	queue *sync.Map

	// Add cache for table/model caching decisions
	cacheDecisions *sync.Map
}

type Config struct {
	Easer  bool
	Cacher Cacher

	CanCachedTables []any
}

func (c *Caches) Name() string {
	return "gorm:caches"
}

func (c *Caches) Initialize(db *gorm.DB) error {
	if c.Conf == nil {
		c.Conf = &Config{
			Easer:  false,
			Cacher: nil,
		}
	}

	if c.Conf.Easer {
		c.queue = &sync.Map{}
	}

	if len(c.Conf.CanCachedTables) > 0 {
		c.cacheDecisions = &sync.Map{}
	}

	callbacks := make(map[queryType]func(db *gorm.DB), 4)
	callbacks[uponQuery] = db.Callback().Query().Get("gorm:query")
	callbacks[uponCreate] = db.Callback().Create().Get("gorm:query")
	callbacks[uponUpdate] = db.Callback().Update().Get("gorm:query")
	callbacks[uponDelete] = db.Callback().Delete().Get("gorm:query")
	c.callbacks = callbacks

	if err := db.Callback().Query().Replace("gorm:query", c.query); err != nil {
		return err
	}

	if err := db.Callback().Create().Replace("gorm:query", c.getMutatorCb(uponCreate)); err != nil {
		return err
	}

	if err := db.Callback().Update().Replace("gorm:query", c.getMutatorCb(uponUpdate)); err != nil {
		return err
	}

	if err := db.Callback().Delete().Replace("gorm:query", c.getMutatorCb(uponDelete)); err != nil {
		return err
	}

	return nil
}

// query is a decorator around the default "gorm:query" callback
// it takes care to both ease database load and cache results
func (c *Caches) query(db *gorm.DB) {
	if c.Conf.Easer == false && c.Conf.Cacher == nil {
		c.callbacks[uponQuery](db)
		return
	}

	identifier := buildIdentifier(db)

	if c.checkCache(db, identifier) {
		return
	}

	c.ease(db, identifier)
	if db.Error != nil {
		return
	}

	c.storeInCache(db, identifier)
	if db.Error != nil {
		return
	}
}

// getMutatorCb returns a decorator which calls the Cacher's Invalidate method
func (c *Caches) getMutatorCb(typ queryType) func(db *gorm.DB) {
	return func(db *gorm.DB) {
		if c.Conf.Cacher != nil {
			if err := c.Conf.Cacher.Invalidate(db.Statement.Context); err != nil {
				_ = db.AddError(err)
			}
		}
		if cb := c.callbacks[typ]; cb != nil { // By default, gorm has no callbacks associated with mutating behaviors
			cb(db)
		}
	}
}

func (c *Caches) ease(db *gorm.DB, identifier string) {
	if c.Conf.Easer == false {
		c.callbacks[uponQuery](db)
		return
	}

	res := ease(&queryTask{
		id:      identifier,
		db:      db,
		queryCb: c.callbacks[uponQuery],
	}, c.queue).(*queryTask)

	if db.Error != nil {
		return
	}

	if res.db.Statement.Dest == db.Statement.Dest {
		return
	}

	detachedQuery := &Query[any]{
		Dest:         db.Statement.Dest,
		RowsAffected: db.Statement.RowsAffected,
	}

	easedQuery := &Query[any]{
		Dest:         res.db.Statement.Dest,
		RowsAffected: res.db.Statement.RowsAffected,
	}
	if err := easedQuery.copyTo(detachedQuery); err != nil {
		_ = db.AddError(err)
	}

	detachedQuery.replaceOn(db)
}

func (c *Caches) checkCache(db *gorm.DB, identifier string) bool {
	if c.Conf.Cacher != nil && c.canCacheTable(db) {
		res, err := c.Conf.Cacher.Get(db.Statement.Context, identifier, &Query[any]{
			Dest:         db.Statement.Dest,
			RowsAffected: db.Statement.RowsAffected,
		})
		if err != nil {
			_ = db.AddError(err)
		}

		if res != nil {
			res.replaceOn(db)
			return true
		}
	}
	return false
}

func (c *Caches) storeInCache(db *gorm.DB, identifier string) {
	if c.Conf.Cacher != nil && c.canCacheTable(db) {
		err := c.Conf.Cacher.Store(db.Statement.Context, identifier, &Query[any]{
			Dest:         db.Statement.Dest,
			RowsAffected: db.Statement.RowsAffected,
		})
		if err != nil {
			_ = db.AddError(err)
		}
	}
}

func (c *Caches) canCacheTable(db *gorm.DB) bool {
	// Fast path - if no tables are specified, cache everything
	if len(c.Conf.CanCachedTables) == 0 {
		return true
	}

	stmt := db.Statement
	if stmt.Model == nil {
		stmt.Model = stmt.Dest
	} else if stmt.Dest == nil {
		stmt.Dest = stmt.Model
	}
	_ = stmt.Parse(stmt.Model)

	tableName := stmt.Table
	if tableName == "" {
		return true
	}
	modelType := reflect.TypeOf(stmt.Model)
	if modelType.Kind() == reflect.Ptr {
		modelType = modelType.Elem()
	}

	// Create a unique key for this table/model combination
	cacheKey := tableName + ":" + modelType.String()

	// Check cache first
	if cached, ok := c.cacheDecisions.Load(cacheKey); ok {
		return cached.(bool)
	}

	// Not in cache, perform full check
	var shouldCache bool
	for _, item := range c.Conf.CanCachedTables {
		switch v := item.(type) {
		case string:
			if matched, err := regexp.MatchString(v, tableName); err == nil && matched {
				shouldCache = true
				break
			}
		default:
			cachedType := reflect.TypeOf(v)
			if cachedType.Kind() == reflect.Ptr {
				cachedType = cachedType.Elem()
			}
			if modelType == cachedType {
				shouldCache = true
				break
			}
		}
	}

	// Store result in cache
	c.cacheDecisions.Store(cacheKey, shouldCache)
	return shouldCache
}

// queryType is used to mark callbacks
type queryType int

const (
	uponQuery queryType = iota
	uponCreate
	uponUpdate
	uponDelete
)
