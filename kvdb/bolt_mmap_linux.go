package kvdb

import "syscall"

// mmapPopulateFlag is the mmap flag that makes the kernel prefault a file,
// reading all of it into the page cache when it is memory mapped.
const mmapPopulateFlag = syscall.MAP_POPULATE
