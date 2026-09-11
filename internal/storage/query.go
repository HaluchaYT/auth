package storage

// Query1 runs fn inside a transaction and returns its value. It exists for
// the handful of upstream call sites that issue a single read or write
// straight on the base connection (models.FindUserByID(db, …),
// db.Create(flowState), …). Outside a transaction such a query runs on the
// connection's default search_path and would bypass tenant scoping; inside
// one it is scoped by the hook in Connection.Transaction like everything
// else.
//
//	user, err := storage.Query1(db, func(tx *storage.Connection) (*models.User, error) {
//		return models.FindUserByID(tx, id)
//	})
func Query1[T any](c *Connection, fn func(tx *Connection) (T, error)) (T, error) {
	var out T
	err := c.Transaction(func(tx *Connection) error {
		var terr error
		out, terr = fn(tx)
		return terr
	})
	return out, err
}

// Exec1 is Query1 for operations that return only an error, such as
// db.Create(x) or db.Destroy(x).
func Exec1(c *Connection, fn func(tx *Connection) error) error {
	return c.Transaction(fn)
}
