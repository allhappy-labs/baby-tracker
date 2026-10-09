package operator

// This file intentionally contains only the exported audit transport types in
// commands.go. Keeping the file reserves the package boundary for the durable
// audit reader that migration 000007 will supply without putting database
// access in the confirmation service.
