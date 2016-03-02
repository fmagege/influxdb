package meta

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/influxdata/influxdb"
	"github.com/influxdata/influxdb/influxql"
	"github.com/influxdata/influxdb/services/meta/internal"
)

//go:generate protoc --gogo_out=. internal/meta.proto

const (
	// DefaultRetentionPolicyReplicaN is the default value of RetentionPolicyInfo.ReplicaN.
	DefaultRetentionPolicyReplicaN = 1

	// DefaultRetentionPolicyDuration is the default value of RetentionPolicyInfo.Duration.
	DefaultRetentionPolicyDuration = 7 * (24 * time.Hour)

	// MinRetentionPolicyDuration represents the minimum duration for a policy.
	MinRetentionPolicyDuration = time.Hour
)

// Data represents the top level collection of all metadata.
type Data struct {
	Term      uint64 // associated raft term
	Index     uint64 // associated raft index
	ClusterID uint64
	MetaNodes []NodeInfo
	DataNodes []NodeInfo
	Databases []DatabaseInfo
	Users     []UserInfo

	MaxNodeID       uint64
	MaxShardGroupID uint64
	MaxShardID      uint64
}

// DataNode returns a node by id.
func (data *Data) DataNode(id uint64) *NodeInfo {
	for i := range data.DataNodes {
		if data.DataNodes[i].ID == id {
			return &data.DataNodes[i]
		}
	}
	return nil
}

// CreateDataNode adds a node to the metadata.
func (data *Data) CreateDataNode(host, tcpHost string) error {
	// Ensure a node with the same host doesn't already exist.
	for _, n := range data.DataNodes {
		if n.TCPHost == tcpHost {
			return ErrNodeExists
		}
	}

	// If an existing meta node exists with the same TCPHost address,
	// then these nodes are actually the same so re-use the existing ID
	var existingID uint64
	for _, n := range data.MetaNodes {
		if n.TCPHost == tcpHost {
			existingID = n.ID
			break
		}
	}

	// We didn't find an existing node, so assign it a new node ID
	if existingID == 0 {
		data.MaxNodeID++
		existingID = data.MaxNodeID
	}

	// Append new node.
	data.DataNodes = append(data.DataNodes, NodeInfo{
		ID:      existingID,
		Host:    host,
		TCPHost: tcpHost,
	})
	sort.Sort(NodeInfos(data.DataNodes))

	return nil
}

// SetDataNode adds a data node with a pre-specified nodeID.
// this should only be used when the cluster is upgrading from 0.9 to 0.10
func (data *Data) SetDataNode(nodeID uint64, host, tcpHost string) error {
	// Ensure a node with the same host doesn't already exist.
	for _, n := range data.DataNodes {
		if n.Host == host {
			return ErrNodeExists
		}
	}

	// Append new node.
	data.DataNodes = append(data.DataNodes, NodeInfo{
		ID:      nodeID,
		Host:    host,
		TCPHost: tcpHost,
	})

	return nil
}

// DeleteDataNode removes a node from the Meta store.
//
// If necessary, DeleteDataNode reassigns ownership of any shards that
// would otherwise become orphaned by the removal of the node from the
// cluster.
func (data *Data) DeleteDataNode(id uint64) error {
	var nodes []NodeInfo

	// Remove the data node from the store's list.
	for _, n := range data.DataNodes {
		if n.ID != id {
			nodes = append(nodes, n)
		}
	}

	if len(nodes) == len(data.DataNodes) {
		return ErrNodeNotFound
	}
	data.DataNodes = nodes

	// Remove node id from all shard infos
	for di, d := range data.Databases {
		for ri, rp := range d.RetentionPolicies {
			for sgi, sg := range rp.ShardGroups {
				var (
					nodeOwnerFreqs = make(map[int]int)
					orphanedShards []ShardInfo
				)
				// Look through all shards in the shard group and
				// determine (1) if a shard no longer has any owners
				// (orphaned); (2) if all shards in the shard group
				// are orphaned; and (3) the number of shards in this
				// group owned by each data node in the cluster.
				for si, s := range sg.Shards {
					// Track of how many shards in the group are
					// owned by each data node in the cluster.
					var nodeIdx = -1
					for i, owner := range s.Owners {
						if owner.NodeID == id {
							nodeIdx = i
						}
						nodeOwnerFreqs[int(owner.NodeID)]++
					}

					if nodeIdx > -1 {
						// Data node owns shard, so relinquish ownership
						// and set new owners on the shard.
						s.Owners = append(s.Owners[:nodeIdx], s.Owners[nodeIdx+1:]...)
						data.Databases[di].RetentionPolicies[ri].ShardGroups[sgi].Shards[si].Owners = s.Owners
					}

					// Shard no longer owned. Will need reassigning
					// an owner.
					if len(s.Owners) == 0 {
						orphanedShards = append(orphanedShards, s)
					}
				}

				// Mark the shard group as deleted if it has no shards,
				// or all of its shards are orphaned.
				if len(sg.Shards) == 0 || len(orphanedShards) == len(sg.Shards) {
					data.Databases[di].RetentionPolicies[ri].ShardGroups[sgi].DeletedAt = time.Now().UTC()
					continue
				}

				// Reassign any orphaned shards. Delete the node we're
				// dropping from the list of potential new owners.
				delete(nodeOwnerFreqs, int(id))

				for _, orphan := range orphanedShards {
					newOwnerID, err := newShardOwner(orphan, nodeOwnerFreqs)
					if err != nil {
						return err
					}

					for si, s := range sg.Shards {
						if s.ID == orphan.ID {
							sg.Shards[si].Owners = append(sg.Shards[si].Owners, ShardOwner{NodeID: newOwnerID})
							data.Databases[di].RetentionPolicies[ri].ShardGroups[sgi].Shards = sg.Shards
							break
						}
					}

				}
			}
		}
	}
	return nil
}

// newShardOwner sets the owner of the provided shard to the data node
// that currently owns the fewest number of shards. If multiple nodes
// own the same (fewest) number of shards, then one of those nodes
// becomes the new shard owner.
func newShardOwner(s ShardInfo, ownerFreqs map[int]int) (uint64, error) {
	var (
		minId   = -1
		minFreq int
	)

	for id, freq := range ownerFreqs {
		if minId == -1 || freq < minFreq {
			minId, minFreq = int(id), freq
		}
	}

	if minId < 0 {
		return 0, fmt.Errorf("cannot reassign shard %d due to lack of data nodes", s.ID)
	}

	// Update the shard owner frequencies and set the new owner on the
	// shard.
	ownerFreqs[minId]++
	return uint64(minId), nil
}

// MetaNode returns a node by id.
func (data *Data) MetaNode(id uint64) *NodeInfo {
	for i := range data.MetaNodes {
		if data.MetaNodes[i].ID == id {
			return &data.MetaNodes[i]
		}
	}
	return nil
}

// CreateMetaNode will add a new meta node to the metastore
func (data *Data) CreateMetaNode(httpAddr, tcpAddr string) error {
	// Ensure a node with the same host doesn't already exist.
	for _, n := range data.MetaNodes {
		if n.Host == httpAddr {
			return ErrNodeExists
		}
	}

	// If an existing data node exists with the same TCPHost address,
	// then these nodes are actually the same so re-use the existing ID
	var existingID uint64
	for _, n := range data.DataNodes {
		if n.TCPHost == tcpAddr {
			existingID = n.ID
			break
		}
	}

	// We didn't find and existing data node ID, so assign a new ID
	// to this meta node.
	if existingID == 0 {
		data.MaxNodeID++
		existingID = data.MaxNodeID
	}

	// Append new node.
	data.MetaNodes = append(data.MetaNodes, NodeInfo{
		ID:      existingID,
		Host:    httpAddr,
		TCPHost: tcpAddr,
	})

	sort.Sort(NodeInfos(data.MetaNodes))
	return nil
}

// SetMetaNode will update the information for the single meta
// node or create a new metanode. If there are more than 1 meta
// nodes already, an error will be returned
func (data *Data) SetMetaNode(httpAddr, tcpAddr string) error {
	if len(data.MetaNodes) > 1 {
		return fmt.Errorf("can't set meta node when there are more than 1 in the metastore")
	}

	if len(data.MetaNodes) == 0 {
		return data.CreateMetaNode(httpAddr, tcpAddr)
	}

	data.MetaNodes[0].Host = httpAddr
	data.MetaNodes[0].TCPHost = tcpAddr

	return nil
}

// DeleteMetaNode will remove the meta node from the store
func (data *Data) DeleteMetaNode(id uint64) error {
	// Node has to be larger than 0 to be real
	if id == 0 {
		return ErrNodeIDRequired
	}

	var nodes []NodeInfo
	for _, n := range data.MetaNodes {
		if n.ID == id {
			continue
		}
		nodes = append(nodes, n)
	}

	if len(nodes) == len(data.MetaNodes) {
		return ErrNodeNotFound
	}

	data.MetaNodes = nodes
	return nil
}

// Database returns a database by name.
func (data *Data) Database(name string) *DatabaseInfo {
	for i := range data.Databases {
		if data.Databases[i].Name == name {
			return &data.Databases[i]
		}
	}
	return nil
}

// CloneDatabases returns a copy of the databases.
func (data *Data) CloneDatabases() []DatabaseInfo {
	if data.Databases == nil {
		return nil
	}
	dbs := make([]DatabaseInfo, len(data.Databases))
	for i := range data.Databases {
		dbs[i] = data.Databases[i].clone()
	}
	return dbs
}

// CreateDatabase creates a new database.
// Returns an error if name is blank or if a database with the same name already exists.
func (data *Data) CreateDatabase(name string) error {
	if name == "" {
		return ErrDatabaseNameRequired
	} else if data.Database(name) != nil {
		return nil
	}

	// Append new node.
	data.Databases = append(data.Databases, DatabaseInfo{Name: name})

	return nil
}

// DropDatabase removes a database by name. It does not return an error
// if the database cannot be found.
func (data *Data) DropDatabase(name string) error {
	for i := range data.Databases {
		if data.Databases[i].Name == name {
			data.Databases = append(data.Databases[:i], data.Databases[i+1:]...)
			break
		}
	}
	return nil
}

// RetentionPolicy returns a retention policy for a database by name.
func (data *Data) RetentionPolicy(database, name string) (*RetentionPolicyInfo, error) {
	di := data.Database(database)
	if di == nil {
		return nil, influxdb.ErrDatabaseNotFound(database)
	}

	for i := range di.RetentionPolicies {
		if di.RetentionPolicies[i].Name == name {
			return &di.RetentionPolicies[i], nil
		}
	}
	return nil, nil
}

// CreateRetentionPolicy creates a new retention policy on a database.
// Returns an error if name is blank or if a database does not exist.
func (data *Data) CreateRetentionPolicy(database string, rpi *RetentionPolicyInfo) error {
	// Validate retention policy.
	if rpi.Name == "" {
		return ErrRetentionPolicyNameRequired
	} else if rpi.ReplicaN < 1 {
		return ErrReplicationFactorTooLow
	}

	// Find database.
	di := data.Database(database)
	if di == nil {
		return influxdb.ErrDatabaseNotFound(database)
	} else if rp := di.RetentionPolicy(rpi.Name); rp != nil {
		// RP with that name already exists.  Make sure they're the same.
		if rp.ReplicaN != rpi.ReplicaN || rp.Duration != rpi.Duration {
			return ErrRetentionPolicyExists
		}
		return nil
	}

	// Append new policy.
	di.RetentionPolicies = append(di.RetentionPolicies, RetentionPolicyInfo{
		Name:               rpi.Name,
		Duration:           rpi.Duration,
		ShardGroupDuration: shardGroupDuration(rpi.Duration),
		ReplicaN:           rpi.ReplicaN,
	})

	return nil
}

// DropRetentionPolicy removes a retention policy from a database by name.
func (data *Data) DropRetentionPolicy(database, name string) error {
	// Find database.
	di := data.Database(database)
	if di == nil {
		// no database? no problem
		return nil
	}

	// Remove from list.
	for i := range di.RetentionPolicies {
		if di.RetentionPolicies[i].Name == name {
			di.RetentionPolicies = append(di.RetentionPolicies[:i], di.RetentionPolicies[i+1:]...)
			break
		}
	}

	return nil
}

// RetentionPolicyUpdate represents retention policy fields to be updated.
type RetentionPolicyUpdate struct {
	Name     *string
	Duration *time.Duration
	ReplicaN *int
}

// SetName sets the RetentionPolicyUpdate.Name
func (rpu *RetentionPolicyUpdate) SetName(v string) { rpu.Name = &v }

// SetDuration sets the RetentionPolicyUpdate.Duration
func (rpu *RetentionPolicyUpdate) SetDuration(v time.Duration) { rpu.Duration = &v }

// SetReplicaN sets the RetentionPolicyUpdate.ReplicaN
func (rpu *RetentionPolicyUpdate) SetReplicaN(v int) { rpu.ReplicaN = &v }

// UpdateRetentionPolicy updates an existing retention policy.
func (data *Data) UpdateRetentionPolicy(database, name string, rpu *RetentionPolicyUpdate) error {
	// Find database.
	di := data.Database(database)
	if di == nil {
		return influxdb.ErrDatabaseNotFound(database)
	}

	// Find policy.
	rpi := di.RetentionPolicy(name)
	if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(name)
	}

	// Ensure new policy doesn't match an existing policy.
	if rpu.Name != nil && *rpu.Name != name && di.RetentionPolicy(*rpu.Name) != nil {
		return ErrRetentionPolicyNameExists
	}

	// Enforce duration of at least MinRetentionPolicyDuration
	if rpu.Duration != nil && *rpu.Duration < MinRetentionPolicyDuration && *rpu.Duration != 0 {
		return ErrRetentionPolicyDurationTooLow
	}

	// Update fields.
	if rpu.Name != nil {
		rpi.Name = *rpu.Name
	}
	if rpu.Duration != nil {
		rpi.Duration = *rpu.Duration
		rpi.ShardGroupDuration = shardGroupDuration(rpi.Duration)
	}
	if rpu.ReplicaN != nil {
		rpi.ReplicaN = *rpu.ReplicaN
	}

	return nil
}

// SetDefaultRetentionPolicy sets the default retention policy for a database.
func (data *Data) SetDefaultRetentionPolicy(database, name string) error {
	// Find database and verify policy exists.
	di := data.Database(database)
	if di == nil {
		return influxdb.ErrDatabaseNotFound(database)
	} else if di.RetentionPolicy(name) == nil {
		return influxdb.ErrRetentionPolicyNotFound(name)
	}

	// Set default policy.
	di.DefaultRetentionPolicy = name

	return nil
}

// ShardGroups returns a list of all shard groups on a database and policy.
func (data *Data) ShardGroups(database, policy string) ([]ShardGroupInfo, error) {
	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return nil, err
	} else if rpi == nil {
		return nil, influxdb.ErrRetentionPolicyNotFound(policy)
	}
	groups := make([]ShardGroupInfo, 0, len(rpi.ShardGroups))
	for _, g := range rpi.ShardGroups {
		if g.Deleted() {
			continue
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// ShardGroupsByTimeRange returns a list of all shard groups on a database and policy that may contain data
// for the specified time range. Shard groups are sorted by start time.
func (data *Data) ShardGroupsByTimeRange(database, policy string, tmin, tmax time.Time) ([]ShardGroupInfo, error) {
	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return nil, err
	} else if rpi == nil {
		return nil, influxdb.ErrRetentionPolicyNotFound(policy)
	}
	groups := make([]ShardGroupInfo, 0, len(rpi.ShardGroups))
	for _, g := range rpi.ShardGroups {
		if g.Deleted() || !g.Overlaps(tmin, tmax) {
			continue
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// ShardGroupByTimestamp returns the shard group on a database and policy for a given timestamp.
func (data *Data) ShardGroupByTimestamp(database, policy string, timestamp time.Time) (*ShardGroupInfo, error) {
	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return nil, err
	} else if rpi == nil {
		return nil, influxdb.ErrRetentionPolicyNotFound(policy)
	}

	return rpi.ShardGroupByTimestamp(timestamp), nil
}

// CreateShardGroup creates a shard group on a database and policy for a given timestamp.
func (data *Data) CreateShardGroup(database, policy string, timestamp time.Time) error {
	// Ensure there are nodes in the metadata.
	if len(data.DataNodes) == 0 {
		return nil
	}

	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return err
	} else if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(policy)
	}

	// Verify that shard group doesn't already exist for this timestamp.
	if rpi.ShardGroupByTimestamp(timestamp) != nil {
		return nil
	}

	// Require at least one replica but no more replicas than nodes.
	replicaN := rpi.ReplicaN
	if replicaN == 0 {
		replicaN = 1
	} else if replicaN > len(data.DataNodes) {
		replicaN = len(data.DataNodes)
	}

	// Determine shard count by node count divided by replication factor.
	// This will ensure nodes will get distributed across nodes evenly and
	// replicated the correct number of times.
	shardN := len(data.DataNodes) / replicaN

	// Create the shard group.
	data.MaxShardGroupID++
	sgi := ShardGroupInfo{}
	sgi.ID = data.MaxShardGroupID
	sgi.StartTime = timestamp.Truncate(rpi.ShardGroupDuration).UTC()
	sgi.EndTime = sgi.StartTime.Add(rpi.ShardGroupDuration).UTC()

	// Create shards on the group.
	sgi.Shards = make([]ShardInfo, shardN)
	for i := range sgi.Shards {
		data.MaxShardID++
		sgi.Shards[i] = ShardInfo{ID: data.MaxShardID}
	}

	// Assign data nodes to shards via round robin.
	// Start from a repeatably "random" place in the node list.
	nodeIndex := int(data.Index % uint64(len(data.DataNodes)))
	for i := range sgi.Shards {
		si := &sgi.Shards[i]
		for j := 0; j < replicaN; j++ {
			nodeID := data.DataNodes[nodeIndex%len(data.DataNodes)].ID
			si.Owners = append(si.Owners, ShardOwner{NodeID: nodeID})
			nodeIndex++
		}
	}

	// Retention policy has a new shard group, so update the policy. Shard
	// Groups must be stored in sorted order, as other parts of the system
	// assume this to be the case.
	rpi.ShardGroups = append(rpi.ShardGroups, sgi)
	sort.Sort(ShardGroupInfos(rpi.ShardGroups))

	return nil
}

// DeleteShardGroup removes a shard group from a database and retention policy by id.
func (data *Data) DeleteShardGroup(database, policy string, id uint64) error {
	// Find retention policy.
	rpi, err := data.RetentionPolicy(database, policy)
	if err != nil {
		return err
	} else if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(policy)
	}

	// Find shard group by ID and set its deletion timestamp.
	for i := range rpi.ShardGroups {
		if rpi.ShardGroups[i].ID == id {
			rpi.ShardGroups[i].DeletedAt = time.Now().UTC()
			return nil
		}
	}

	return ErrShardGroupNotFound
}

// CreateContinuousQuery adds a named continuous query to a database.
func (data *Data) CreateContinuousQuery(database, name, query string) error {
	di := data.Database(database)
	if di == nil {
		return influxdb.ErrDatabaseNotFound(database)
	}

	// Ensure the name doesn't already exist.
	for i := range di.ContinuousQueries {
		if di.ContinuousQueries[i].Name == name {
			return ErrContinuousQueryExists
		}
	}

	// Append new query.
	di.ContinuousQueries = append(di.ContinuousQueries, ContinuousQueryInfo{
		Name:  name,
		Query: query,
	})

	return nil
}

// DropContinuousQuery removes a continuous query.
func (data *Data) DropContinuousQuery(database, name string) error {
	di := data.Database(database)
	if di == nil {
		return influxdb.ErrDatabaseNotFound(database)
	}

	for i := range di.ContinuousQueries {
		if di.ContinuousQueries[i].Name == name {
			di.ContinuousQueries = append(di.ContinuousQueries[:i], di.ContinuousQueries[i+1:]...)
			return nil
		}
	}
	return ErrContinuousQueryNotFound
}

// CreateSubscription adds a named subscription to a database and retention policy.
func (data *Data) CreateSubscription(database, rp, name, mode string, destinations []string) error {
	rpi, err := data.RetentionPolicy(database, rp)
	if err != nil {
		return err
	} else if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(rp)
	}

	// Ensure the name doesn't already exist.
	for i := range rpi.Subscriptions {
		if rpi.Subscriptions[i].Name == name {
			return ErrSubscriptionExists
		}
	}

	// Append new query.
	rpi.Subscriptions = append(rpi.Subscriptions, SubscriptionInfo{
		Name:         name,
		Mode:         mode,
		Destinations: destinations,
	})

	return nil
}

// DropSubscription removes a subscription.
func (data *Data) DropSubscription(database, rp, name string) error {
	rpi, err := data.RetentionPolicy(database, rp)
	if err != nil {
		return err
	} else if rpi == nil {
		return influxdb.ErrRetentionPolicyNotFound(rp)
	}

	for i := range rpi.Subscriptions {
		if rpi.Subscriptions[i].Name == name {
			rpi.Subscriptions = append(rpi.Subscriptions[:i], rpi.Subscriptions[i+1:]...)
			return nil
		}
	}
	return ErrSubscriptionNotFound
}

// User returns a user by username.
func (data *Data) User(username string) *UserInfo {
	for i := range data.Users {
		if data.Users[i].Name == username {
			return &data.Users[i]
		}
	}
	return nil
}

// CreateUser creates a new user.
func (data *Data) CreateUser(name, hash string, admin bool) error {
	// Ensure the user doesn't already exist.
	if name == "" {
		return ErrUsernameRequired
	} else if data.User(name) != nil {
		return ErrUserExists
	}

	// Append new user.
	data.Users = append(data.Users, UserInfo{
		Name:  name,
		Hash:  hash,
		Admin: admin,
	})

	return nil
}

// DropUser removes an existing user by name.
func (data *Data) DropUser(name string) error {
	for i := range data.Users {
		if data.Users[i].Name == name {
			data.Users = append(data.Users[:i], data.Users[i+1:]...)
			return nil
		}
	}
	return ErrUserNotFound
}

// UpdateUser updates the password hash of an existing user.
func (data *Data) UpdateUser(name, hash string) error {
	for i := range data.Users {
		if data.Users[i].Name == name {
			data.Users[i].Hash = hash
			return nil
		}
	}
	return ErrUserNotFound
}

// SetPrivilege sets a privilege for a user on a database.
func (data *Data) SetPrivilege(name, database string, p influxql.Privilege) error {
	ui := data.User(name)
	if ui == nil {
		return ErrUserNotFound
	}

	if ui.Privileges == nil {
		ui.Privileges = make(map[string]influxql.Privilege)
	}
	ui.Privileges[database] = p

	return nil
}

// SetAdminPrivilege sets the admin privilege for a user.
func (data *Data) SetAdminPrivilege(name string, admin bool) error {
	ui := data.User(name)
	if ui == nil {
		return ErrUserNotFound
	}

	ui.Admin = admin

	return nil
}

// UserPrivileges gets the privileges for a user.
func (data *Data) UserPrivileges(name string) (map[string]influxql.Privilege, error) {
	ui := data.User(name)
	if ui == nil {
		return nil, ErrUserNotFound
	}

	return ui.Privileges, nil
}

// UserPrivilege gets the privilege for a user on a database.
func (data *Data) UserPrivilege(name, database string) (*influxql.Privilege, error) {
	ui := data.User(name)
	if ui == nil {
		return nil, ErrUserNotFound
	}

	for db, p := range ui.Privileges {
		if db == database {
			return &p, nil
		}
	}

	return influxql.NewPrivilege(influxql.NoPrivileges), nil
}

// Clone returns a copy of data with a new version.
func (data *Data) Clone() *Data {
	other := *data

	// Copy nodes.
	if data.DataNodes != nil {
		other.DataNodes = make([]NodeInfo, len(data.DataNodes))
		for i := range data.DataNodes {
			other.DataNodes[i] = data.DataNodes[i].clone()
		}
	}

	if data.MetaNodes != nil {
		other.MetaNodes = make([]NodeInfo, len(data.MetaNodes))
		for i := range data.MetaNodes {
			other.MetaNodes[i] = data.MetaNodes[i].clone()
		}
	}

	// Deep copy databases.
	if data.Databases != nil {
		other.Databases = make([]DatabaseInfo, len(data.Databases))
		for i := range data.Databases {
			other.Databases[i] = data.Databases[i].clone()
		}
	}

	// Copy users.
	if data.Users != nil {
		other.Users = make([]UserInfo, len(data.Users))
		for i := range data.Users {
			other.Users[i] = data.Users[i].clone()
		}
	}

	return &other
}

// marshal serializes to a protobuf representation.
func (data *Data) marshal() *internal.Data {
	pb := &internal.Data{
		Term:      proto.Uint64(data.Term),
		Index:     proto.Uint64(data.Index),
		ClusterID: proto.Uint64(data.ClusterID),

		MaxNodeID:       proto.Uint64(data.MaxNodeID),
		MaxShardGroupID: proto.Uint64(data.MaxShardGroupID),
		MaxShardID:      proto.Uint64(data.MaxShardID),
	}

	pb.DataNodes = make([]*internal.NodeInfo, len(data.DataNodes))
	for i := range data.DataNodes {
		pb.DataNodes[i] = data.DataNodes[i].marshal()
	}

	pb.MetaNodes = make([]*internal.NodeInfo, len(data.MetaNodes))
	for i := range data.MetaNodes {
		pb.MetaNodes[i] = data.MetaNodes[i].marshal()
	}

	pb.Databases = make([]*internal.DatabaseInfo, len(data.Databases))
	for i := range data.Databases {
		pb.Databases[i] = data.Databases[i].marshal()
	}

	pb.Users = make([]*internal.UserInfo, len(data.Users))
	for i := range data.Users {
		pb.Users[i] = data.Users[i].marshal()
	}

	return pb
}

// unmarshal deserializes from a protobuf representation.
func (data *Data) unmarshal(pb *internal.Data) {
	data.Term = pb.GetTerm()
	data.Index = pb.GetIndex()
	data.ClusterID = pb.GetClusterID()

	data.MaxNodeID = pb.GetMaxNodeID()
	data.MaxShardGroupID = pb.GetMaxShardGroupID()
	data.MaxShardID = pb.GetMaxShardID()

	// TODO: Nodes is deprecated. This is being left here to make migration from 0.9.x to 0.10.0 possible
	if len(pb.GetNodes()) > 0 {
		data.DataNodes = make([]NodeInfo, len(pb.GetNodes()))
		for i, x := range pb.GetNodes() {
			data.DataNodes[i].unmarshal(x)
		}
	} else {
		data.DataNodes = make([]NodeInfo, len(pb.GetDataNodes()))
		for i, x := range pb.GetDataNodes() {
			data.DataNodes[i].unmarshal(x)
		}
	}

	data.MetaNodes = make([]NodeInfo, len(pb.GetMetaNodes()))
	for i, x := range pb.GetMetaNodes() {
		data.MetaNodes[i].unmarshal(x)
	}

	data.Databases = make([]DatabaseInfo, len(pb.GetDatabases()))
	for i, x := range pb.GetDatabases() {
		data.Databases[i].unmarshal(x)
	}

	data.Users = make([]UserInfo, len(pb.GetUsers()))
	for i, x := range pb.GetUsers() {
		data.Users[i].unmarshal(x)
	}
}

// MarshalBinary encodes the metadata to a binary format.
func (data *Data) MarshalBinary() ([]byte, error) {
	return proto.Marshal(data.marshal())
}

// UnmarshalBinary decodes the object from a binary format.
func (data *Data) UnmarshalBinary(buf []byte) error {
	var pb internal.Data
	if err := proto.Unmarshal(buf, &pb); err != nil {
		return err
	}
	data.unmarshal(&pb)
	return nil
}

// NodeInfo represents information about a single node in the cluster.
type NodeInfo struct {
	ID      uint64
	Host    string
	TCPHost string
}

// clone returns a deep copy of ni.
func (ni NodeInfo) clone() NodeInfo { return ni }

// marshal serializes to a protobuf representation.
func (ni NodeInfo) marshal() *internal.NodeInfo {
	pb := &internal.NodeInfo{}
	pb.ID = proto.Uint64(ni.ID)
	pb.Host = proto.String(ni.Host)
	pb.TCPHost = proto.String(ni.TCPHost)
	return pb
}

// unmarshal deserializes from a protobuf representation.
func (ni *NodeInfo) unmarshal(pb *internal.NodeInfo) {
	ni.ID = pb.GetID()
	ni.Host = pb.GetHost()
	ni.TCPHost = pb.GetTCPHost()
}

// NodeInfos is a slice of NodeInfo used for sorting
type NodeInfos []NodeInfo

func (n NodeInfos) Len() int           { return len(n) }
func (n NodeInfos) Swap(i, j int)      { n[i], n[j] = n[j], n[i] }
func (n NodeInfos) Less(i, j int) bool { return n[i].ID < n[j].ID }

// DatabaseInfo represents information about a database in the system.
type DatabaseInfo struct {
	Name                   string
	DefaultRetentionPolicy string
	RetentionPolicies      []RetentionPolicyInfo
	ContinuousQueries      []ContinuousQueryInfo
}

// RetentionPolicy returns a retention policy by name.
func (di DatabaseInfo) RetentionPolicy(name string) *RetentionPolicyInfo {
	for i := range di.RetentionPolicies {
		if di.RetentionPolicies[i].Name == name {
			return &di.RetentionPolicies[i]
		}
	}
	return nil
}

// ShardInfos returns a list of all shards' info for the database.
func (di DatabaseInfo) ShardInfos() []ShardInfo {
	shards := map[uint64]*ShardInfo{}
	for i := range di.RetentionPolicies {
		for j := range di.RetentionPolicies[i].ShardGroups {
			sg := di.RetentionPolicies[i].ShardGroups[j]
			// Skip deleted shard groups
			if sg.Deleted() {
				continue
			}
			for k := range sg.Shards {
				si := &di.RetentionPolicies[i].ShardGroups[j].Shards[k]
				shards[si.ID] = si
			}
		}
	}

	infos := make([]ShardInfo, 0, len(shards))
	for _, info := range shards {
		infos = append(infos, *info)
	}

	return infos
}

// clone returns a deep copy of di.
func (di DatabaseInfo) clone() DatabaseInfo {
	other := di

	if di.RetentionPolicies != nil {
		other.RetentionPolicies = make([]RetentionPolicyInfo, len(di.RetentionPolicies))
		for i := range di.RetentionPolicies {
			other.RetentionPolicies[i] = di.RetentionPolicies[i].clone()
		}
	}

	// Copy continuous queries.
	if di.ContinuousQueries != nil {
		other.ContinuousQueries = make([]ContinuousQueryInfo, len(di.ContinuousQueries))
		for i := range di.ContinuousQueries {
			other.ContinuousQueries[i] = di.ContinuousQueries[i].clone()
		}
	}

	return other
}

// marshal serializes to a protobuf representation.
func (di DatabaseInfo) marshal() *internal.DatabaseInfo {
	pb := &internal.DatabaseInfo{}
	pb.Name = proto.String(di.Name)
	pb.DefaultRetentionPolicy = proto.String(di.DefaultRetentionPolicy)

	pb.RetentionPolicies = make([]*internal.RetentionPolicyInfo, len(di.RetentionPolicies))
	for i := range di.RetentionPolicies {
		pb.RetentionPolicies[i] = di.RetentionPolicies[i].marshal()
	}

	pb.ContinuousQueries = make([]*internal.ContinuousQueryInfo, len(di.ContinuousQueries))
	for i := range di.ContinuousQueries {
		pb.ContinuousQueries[i] = di.ContinuousQueries[i].marshal()
	}
	return pb
}

// unmarshal deserializes from a protobuf representation.
func (di *DatabaseInfo) unmarshal(pb *internal.DatabaseInfo) {
	di.Name = pb.GetName()
	di.DefaultRetentionPolicy = pb.GetDefaultRetentionPolicy()

	if len(pb.GetRetentionPolicies()) > 0 {
		di.RetentionPolicies = make([]RetentionPolicyInfo, len(pb.GetRetentionPolicies()))
		for i, x := range pb.GetRetentionPolicies() {
			di.RetentionPolicies[i].unmarshal(x)
		}
	}

	if len(pb.GetContinuousQueries()) > 0 {
		di.ContinuousQueries = make([]ContinuousQueryInfo, len(pb.GetContinuousQueries()))
		for i, x := range pb.GetContinuousQueries() {
			di.ContinuousQueries[i].unmarshal(x)
		}
	}
}

// RetentionPolicyInfo represents metadata about a retention policy.
type RetentionPolicyInfo struct {
	Name               string
	ReplicaN           int
	Duration           time.Duration
	ShardGroupDuration time.Duration
	ShardGroups        []ShardGroupInfo
	Subscriptions      []SubscriptionInfo
}

// NewRetentionPolicyInfo returns a new instance of RetentionPolicyInfo with defaults set.
func NewRetentionPolicyInfo(name string) *RetentionPolicyInfo {
	return &RetentionPolicyInfo{
		Name:     name,
		ReplicaN: DefaultRetentionPolicyReplicaN,
		Duration: DefaultRetentionPolicyDuration,
	}
}

// ShardGroupByTimestamp returns the shard group in the policy that contains the timestamp.
func (rpi *RetentionPolicyInfo) ShardGroupByTimestamp(timestamp time.Time) *ShardGroupInfo {
	for i := range rpi.ShardGroups {
		if rpi.ShardGroups[i].Contains(timestamp) && !rpi.ShardGroups[i].Deleted() {
			return &rpi.ShardGroups[i]
		}
	}
	return nil
}

// ExpiredShardGroups returns the Shard Groups which are considered expired, for the given time.
func (rpi *RetentionPolicyInfo) ExpiredShardGroups(t time.Time) []*ShardGroupInfo {
	var groups = make([]*ShardGroupInfo, 0)
	for i := range rpi.ShardGroups {
		if rpi.ShardGroups[i].Deleted() {
			continue
		}
		if rpi.Duration != 0 && rpi.ShardGroups[i].EndTime.Add(rpi.Duration).Before(t) {
			groups = append(groups, &rpi.ShardGroups[i])
		}
	}
	return groups
}

// DeletedShardGroups returns the Shard Groups which are marked as deleted.
func (rpi *RetentionPolicyInfo) DeletedShardGroups() []*ShardGroupInfo {
	var groups = make([]*ShardGroupInfo, 0)
	for i := range rpi.ShardGroups {
		if rpi.ShardGroups[i].Deleted() {
			groups = append(groups, &rpi.ShardGroups[i])
		}
	}
	return groups
}

// marshal serializes to a protobuf representation.
func (rpi *RetentionPolicyInfo) marshal() *internal.RetentionPolicyInfo {
	pb := &internal.RetentionPolicyInfo{
		Name:               proto.String(rpi.Name),
		ReplicaN:           proto.Uint32(uint32(rpi.ReplicaN)),
		Duration:           proto.Int64(int64(rpi.Duration)),
		ShardGroupDuration: proto.Int64(int64(rpi.ShardGroupDuration)),
	}

	pb.ShardGroups = make([]*internal.ShardGroupInfo, len(rpi.ShardGroups))
	for i, sgi := range rpi.ShardGroups {
		pb.ShardGroups[i] = sgi.marshal()
	}

	pb.Subscriptions = make([]*internal.SubscriptionInfo, len(rpi.Subscriptions))
	for i, sub := range rpi.Subscriptions {
		pb.Subscriptions[i] = sub.marshal()
	}

	return pb
}

// unmarshal deserializes from a protobuf representation.
func (rpi *RetentionPolicyInfo) unmarshal(pb *internal.RetentionPolicyInfo) {
	rpi.Name = pb.GetName()
	rpi.ReplicaN = int(pb.GetReplicaN())
	rpi.Duration = time.Duration(pb.GetDuration())
	rpi.ShardGroupDuration = time.Duration(pb.GetShardGroupDuration())

	if len(pb.GetShardGroups()) > 0 {
		rpi.ShardGroups = make([]ShardGroupInfo, len(pb.GetShardGroups()))
		for i, x := range pb.GetShardGroups() {
			rpi.ShardGroups[i].unmarshal(x)
		}
	}
	if len(pb.GetSubscriptions()) > 0 {
		rpi.Subscriptions = make([]SubscriptionInfo, len(pb.GetSubscriptions()))
		for i, x := range pb.GetSubscriptions() {
			rpi.Subscriptions[i].unmarshal(x)
		}
	}
}

// clone returns a deep copy of rpi.
func (rpi RetentionPolicyInfo) clone() RetentionPolicyInfo {
	other := rpi

	if rpi.ShardGroups != nil {
		other.ShardGroups = make([]ShardGroupInfo, len(rpi.ShardGroups))
		for i := range rpi.ShardGroups {
			other.ShardGroups[i] = rpi.ShardGroups[i].clone()
		}
	}

	return other
}

// MarshalBinary encodes rpi to a binary format.
func (rpi *RetentionPolicyInfo) MarshalBinary() ([]byte, error) {
	return proto.Marshal(rpi.marshal())
}

// UnmarshalBinary decodes rpi from a binary format.
func (rpi *RetentionPolicyInfo) UnmarshalBinary(data []byte) error {
	var pb internal.RetentionPolicyInfo
	if err := proto.Unmarshal(data, &pb); err != nil {
		return err
	}
	rpi.unmarshal(&pb)
	return nil
}

// shardGroupDuration returns the duration for a shard group based on a policy duration.
func shardGroupDuration(d time.Duration) time.Duration {
	if d >= 180*24*time.Hour || d == 0 { // 6 months or 0
		return 7 * 24 * time.Hour
	} else if d >= 2*24*time.Hour { // 2 days
		return 1 * 24 * time.Hour
	}
	return 1 * time.Hour
}

// ShardGroupInfo represents metadata about a shard group. The DeletedAt field is important
// because it makes it clear that a ShardGroup has been marked as deleted, and allow the system
// to be sure that a ShardGroup is not simply missing. If the DeletedAt is set, the system can
// safely delete any associated shards.
type ShardGroupInfo struct {
	ID        uint64
	StartTime time.Time
	EndTime   time.Time
	DeletedAt time.Time
	Shards    []ShardInfo
}

// ShardGroupInfos implements sort.Interface on []ShardGroupInfo, based
// on the StartTime field.
type ShardGroupInfos []ShardGroupInfo

func (a ShardGroupInfos) Len() int           { return len(a) }
func (a ShardGroupInfos) Less(i, j int) bool { return a[i].StartTime.Before(a[j].StartTime) }
func (a ShardGroupInfos) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// Contains return true if the shard group contains data for the timestamp.
func (sgi *ShardGroupInfo) Contains(timestamp time.Time) bool {
	return !sgi.StartTime.After(timestamp) && sgi.EndTime.After(timestamp)
}

// Overlaps return whether the shard group contains data for the time range between min and max
func (sgi *ShardGroupInfo) Overlaps(min, max time.Time) bool {
	return !sgi.StartTime.After(max) && sgi.EndTime.After(min)
}

// Deleted returns whether this ShardGroup has been deleted.
func (sgi *ShardGroupInfo) Deleted() bool {
	return !sgi.DeletedAt.IsZero()
}

// clone returns a deep copy of sgi.
func (sgi ShardGroupInfo) clone() ShardGroupInfo {
	other := sgi

	if sgi.Shards != nil {
		other.Shards = make([]ShardInfo, len(sgi.Shards))
		for i := range sgi.Shards {
			other.Shards[i] = sgi.Shards[i].clone()
		}
	}

	return other
}

// ShardFor returns the ShardInfo for a Point hash
func (sgi *ShardGroupInfo) ShardFor(hash uint64) ShardInfo {
	return sgi.Shards[hash%uint64(len(sgi.Shards))]
}

// marshal serializes to a protobuf representation.
func (sgi *ShardGroupInfo) marshal() *internal.ShardGroupInfo {
	pb := &internal.ShardGroupInfo{
		ID:        proto.Uint64(sgi.ID),
		StartTime: proto.Int64(MarshalTime(sgi.StartTime)),
		EndTime:   proto.Int64(MarshalTime(sgi.EndTime)),
		DeletedAt: proto.Int64(MarshalTime(sgi.DeletedAt)),
	}

	pb.Shards = make([]*internal.ShardInfo, len(sgi.Shards))
	for i := range sgi.Shards {
		pb.Shards[i] = sgi.Shards[i].marshal()
	}

	return pb
}

// unmarshal deserializes from a protobuf representation.
func (sgi *ShardGroupInfo) unmarshal(pb *internal.ShardGroupInfo) {
	sgi.ID = pb.GetID()
	sgi.StartTime = UnmarshalTime(pb.GetStartTime())
	sgi.EndTime = UnmarshalTime(pb.GetEndTime())
	sgi.DeletedAt = UnmarshalTime(pb.GetDeletedAt())

	if len(pb.GetShards()) > 0 {
		sgi.Shards = make([]ShardInfo, len(pb.GetShards()))
		for i, x := range pb.GetShards() {
			sgi.Shards[i].unmarshal(x)
		}
	}
}

// ShardInfo represents metadata about a shard.
type ShardInfo struct {
	ID     uint64
	Owners []ShardOwner
}

// OwnedBy determines whether the shard's owner IDs includes nodeID.
func (si ShardInfo) OwnedBy(nodeID uint64) bool {
	for _, so := range si.Owners {
		if so.NodeID == nodeID {
			return true
		}
	}
	return false
}

// clone returns a deep copy of si.
func (si ShardInfo) clone() ShardInfo {
	other := si

	if si.Owners != nil {
		other.Owners = make([]ShardOwner, len(si.Owners))
		for i := range si.Owners {
			other.Owners[i] = si.Owners[i].clone()
		}
	}

	return other
}

// marshal serializes to a protobuf representation.
func (si ShardInfo) marshal() *internal.ShardInfo {
	pb := &internal.ShardInfo{
		ID: proto.Uint64(si.ID),
	}

	pb.Owners = make([]*internal.ShardOwner, len(si.Owners))
	for i := range si.Owners {
		pb.Owners[i] = si.Owners[i].marshal()
	}

	return pb
}

// UnmarshalBinary decodes the object from a binary format.
func (si *ShardInfo) UnmarshalBinary(buf []byte) error {
	var pb internal.ShardInfo
	if err := proto.Unmarshal(buf, &pb); err != nil {
		return err
	}
	si.unmarshal(&pb)
	return nil
}

// unmarshal deserializes from a protobuf representation.
func (si *ShardInfo) unmarshal(pb *internal.ShardInfo) {
	si.ID = pb.GetID()

	// If deprecated "OwnerIDs" exists then convert it to "Owners" format.
	if len(pb.GetOwnerIDs()) > 0 {
		si.Owners = make([]ShardOwner, len(pb.GetOwnerIDs()))
		for i, x := range pb.GetOwnerIDs() {
			si.Owners[i].unmarshal(&internal.ShardOwner{
				NodeID: proto.Uint64(x),
			})
		}
	} else if len(pb.GetOwners()) > 0 {
		si.Owners = make([]ShardOwner, len(pb.GetOwners()))
		for i, x := range pb.GetOwners() {
			si.Owners[i].unmarshal(x)
		}
	}
}

// SubscriptionInfo hold the subscription information
type SubscriptionInfo struct {
	Name         string
	Mode         string
	Destinations []string
}

// marshal serializes to a protobuf representation.
func (si SubscriptionInfo) marshal() *internal.SubscriptionInfo {
	pb := &internal.SubscriptionInfo{
		Name: proto.String(si.Name),
		Mode: proto.String(si.Mode),
	}

	pb.Destinations = make([]string, len(si.Destinations))
	for i := range si.Destinations {
		pb.Destinations[i] = si.Destinations[i]
	}
	return pb
}

// unmarshal deserializes from a protobuf representation.
func (si *SubscriptionInfo) unmarshal(pb *internal.SubscriptionInfo) {
	si.Name = pb.GetName()
	si.Mode = pb.GetMode()

	if len(pb.GetDestinations()) > 0 {
		si.Destinations = make([]string, len(pb.GetDestinations()))
		for i, h := range pb.GetDestinations() {
			si.Destinations[i] = h
		}
	}
}

// ShardOwner represents a node that owns a shard.
type ShardOwner struct {
	NodeID uint64
}

// clone returns a deep copy of so.
func (so ShardOwner) clone() ShardOwner {
	return so
}

// marshal serializes to a protobuf representation.
func (so ShardOwner) marshal() *internal.ShardOwner {
	return &internal.ShardOwner{
		NodeID: proto.Uint64(so.NodeID),
	}
}

// unmarshal deserializes from a protobuf representation.
func (so *ShardOwner) unmarshal(pb *internal.ShardOwner) {
	so.NodeID = pb.GetNodeID()
}

// ContinuousQueryInfo represents metadata about a continuous query.
type ContinuousQueryInfo struct {
	Name  string
	Query string
}

// clone returns a deep copy of cqi.
func (cqi ContinuousQueryInfo) clone() ContinuousQueryInfo { return cqi }

// marshal serializes to a protobuf representation.
func (cqi ContinuousQueryInfo) marshal() *internal.ContinuousQueryInfo {
	return &internal.ContinuousQueryInfo{
		Name:  proto.String(cqi.Name),
		Query: proto.String(cqi.Query),
	}
}

// unmarshal deserializes from a protobuf representation.
func (cqi *ContinuousQueryInfo) unmarshal(pb *internal.ContinuousQueryInfo) {
	cqi.Name = pb.GetName()
	cqi.Query = pb.GetQuery()
}

// UserInfo represents metadata about a user in the system.
type UserInfo struct {
	Name       string
	Hash       string
	Admin      bool
	Privileges map[string]influxql.Privilege
}

// Authorize returns true if the user is authorized and false if not.
func (ui *UserInfo) Authorize(privilege influxql.Privilege, database string) bool {
	if ui.Admin {
		return true
	}
	p, ok := ui.Privileges[database]
	return ok && (p == privilege || p == influxql.AllPrivileges)
}

// clone returns a deep copy of si.
func (ui UserInfo) clone() UserInfo {
	other := ui

	if ui.Privileges != nil {
		other.Privileges = make(map[string]influxql.Privilege)
		for k, v := range ui.Privileges {
			other.Privileges[k] = v
		}
	}

	return other
}

// marshal serializes to a protobuf representation.
func (ui UserInfo) marshal() *internal.UserInfo {
	pb := &internal.UserInfo{
		Name:  proto.String(ui.Name),
		Hash:  proto.String(ui.Hash),
		Admin: proto.Bool(ui.Admin),
	}

	for database, privilege := range ui.Privileges {
		pb.Privileges = append(pb.Privileges, &internal.UserPrivilege{
			Database:  proto.String(database),
			Privilege: proto.Int32(int32(privilege)),
		})
	}

	return pb
}

// unmarshal deserializes from a protobuf representation.
func (ui *UserInfo) unmarshal(pb *internal.UserInfo) {
	ui.Name = pb.GetName()
	ui.Hash = pb.GetHash()
	ui.Admin = pb.GetAdmin()

	ui.Privileges = make(map[string]influxql.Privilege)
	for _, p := range pb.GetPrivileges() {
		ui.Privileges[p.GetDatabase()] = influxql.Privilege(p.GetPrivilege())
	}
}

type Lease struct {
	Name       string    `json:"name"`
	Expiration time.Time `json:"expiration"`
	Owner      uint64    `json:"owner"`
}

type Leases struct {
	mu sync.Mutex
	m  map[string]*Lease
	d  time.Duration
}

func NewLeases(d time.Duration) *Leases {
	return &Leases{
		m: make(map[string]*Lease),
		d: d,
	}
}

func (leases *Leases) Acquire(name string, nodeID uint64) (*Lease, error) {
	leases.mu.Lock()
	defer leases.mu.Unlock()

	l, ok := leases.m[name]
	if ok {
		if time.Now().After(l.Expiration) || l.Owner == nodeID {
			l.Expiration = time.Now().Add(leases.d)
			l.Owner = nodeID
			return l, nil
		}
		return l, errors.New("another node has the lease")
	}

	l = &Lease{
		Name:       name,
		Expiration: time.Now().Add(leases.d),
		Owner:      nodeID,
	}

	leases.m[name] = l

	return l, nil
}

// MarshalTime converts t to nanoseconds since epoch. A zero time returns 0.
func MarshalTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// UnmarshalTime converts nanoseconds since epoch to time.
// A zero value returns a zero time.
func UnmarshalTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}
