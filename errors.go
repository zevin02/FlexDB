package FlexDB

import "errors"

var (
	ErrKeyIsEmpty            = errors.New("the key is empty")
	ErrIndexUpdateFailed     = errors.New("failed to update index")
	ErrKeyNotFound           = errors.New("the key is not found in database")
	ErrDataFileNotFound      = errors.New("data file is not found in database")
	ErrDirIsInValid          = errors.New("DirPath is invalid")
	ErrFileSizeInValid       = errors.New("FileSize is invalid")
	ErrDataDirCorrupted      = errors.New("database directory maybe corrupted")
	ErrExceedMaxBatchNum     = errors.New("exceed the max batch num")
	ErrMergeIsProgress       = errors.New("merge is progress")
	ErrDataBaseIsUsing       = errors.New("database is using")
	ErrMergeRatio            = errors.New("invalid merge ratio,must between 0 and 1")
	ErrMergeRatioUnReached   = errors.New("the merge ratio do not reach the ratio")
	ErrNoEnoughSpaceForMerge = errors.New("not enough space for merge")
)
