package db

// SyncedAdminKeyPrefix prefixes the settings rows that record which
// initial_admin_keys the migration attached to an admin user, so it can
// remove exactly those keys when they leave the config. The rest of the row
// key is the authorized key and the value is the id of the user the key was
// attached to. Adding or removing a key with the user commands deletes its
// row, so the key is then kept or removed only by hand.
const SyncedAdminKeyPrefix = "initial_admin_key:"
