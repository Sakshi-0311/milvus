package dataservice

import (
	"errors"
	"path"
	"strconv"

	"github.com/golang/protobuf/proto"
	"github.com/milvus-io/milvus/internal/proto/datapb"
)

// binlog helper functions persisting binlog paths into kv storage.
// migrated from datanode[Author XuanYang-cn]
// ddl binlog etcd meta key:
//   ${prefix}/${collectionID}/${idx}
// segment binlog etcd meta key:
//   ${prefix}/${segmentID}/${fieldID}/${idx}

// genKey gives a valid key string for lists of UniqueIDs:
//  if alloc is true, the returned keys will have a generated-unique ID at the end.
//  if alloc is false, the returned keys will only consist of provided ids.
func (s *Server) genKey(alloc bool, ids ...UniqueID) (key string, err error) {
	if alloc {
		idx, err := s.allocator.allocID()
		if err != nil {
			return "", err
		}
		ids = append(ids, idx)
	}

	idStr := make([]string, len(ids))
	for _, id := range ids {
		idStr = append(idStr, strconv.FormatInt(id, 10))
	}

	key = path.Join(idStr...)
	return key, nil
}

var (
	errNilKvClient = errors.New("kv client not initialized")
)

//SaveBinLogMetaTxn saves segment-field2Path, collection-tsPath/ddlPath into kv store in transcation
func (s *Server) SaveBinLogMetaTxn(meta map[string]string) error {
	if s.kvClient == nil {
		return errNilKvClient
	}
	return s.kvClient.MultiSave(meta)
}

var (
	errNilID2Paths = errors.New("nil ID2PathList")
)

// prepareField2PathMeta parses fields2Paths ID2PathList
//		into key-value for kv store
func (s *Server) prepareField2PathMeta(segID UniqueID, field2Paths *datapb.ID2PathList) (result map[string]string, err error) {
	if field2Paths == nil {
		return nil, errNilID2Paths
	}
	result = make(map[string]string, len(field2Paths.GetPaths()))
	fieldID := field2Paths.GetID()
	var key string
	for _, p := range field2Paths.GetPaths() {
		key, err = s.genKey(true, segID, fieldID)
		if err != nil {
			return nil, err
		}
		binlogPath := proto.MarshalTextString(&datapb.SegmentFieldBinlogMeta{
			FieldID:    fieldID,
			BinlogPath: p,
		})

		result[path.Join(Params.SegmentBinlogSubPath, key)] = binlogPath
	}
	return result, err
}

// prepareDDLBinlogMeta parses Coll2DdlBinlogPaths & Coll2TsBinlogPaths
//		into key-value for kv store
func (s *Server) prepareDDLBinlogMeta(collID UniqueID, ddlMetas []*datapb.DDLBinlogMeta) (result map[string]string, err error) {
	if ddlMetas == nil {
		return nil, errNilID2Paths
	}

	result = make(map[string]string, len(ddlMetas))

	for _, ddlMeta := range ddlMetas {
		if ddlMeta == nil {
			continue
		}
		uniqueKey, err := s.genKey(true, collID)
		if err != nil {
			return nil, err
		}
		binlogPathPair := proto.MarshalTextString(ddlMeta)

		result[path.Join(Params.CollectionBinlogSubPath, uniqueKey)] = binlogPathPair
	}
	return result, nil
}

// getFieldBinlogMeta querys field binlog meta from kv store
func (s *Server) getFieldBinlogMeta(segmentID UniqueID,
	fieldID UniqueID) (metas []*datapb.SegmentFieldBinlogMeta, err error) {

	prefix, err := s.genKey(false, segmentID, fieldID)
	if err != nil {
		return nil, err
	}

	_, vs, err := s.kvClient.LoadWithPrefix(path.Join(Params.SegmentBinlogSubPath, prefix))
	if err != nil {
		return nil, err
	}

	for _, blob := range vs {
		m := &datapb.SegmentFieldBinlogMeta{}
		if err = proto.UnmarshalText(blob, m); err != nil {
			return nil, err
		}

		metas = append(metas, m)
	}

	return
}

// getSegmentBinlogMeta querys segment bin log meta from kv store
func (s *Server) getSegmentBinlogMeta(segmentID UniqueID) (metas []*datapb.SegmentFieldBinlogMeta, err error) {

	prefix, err := s.genKey(false, segmentID)
	if err != nil {
		return nil, err
	}

	_, vs, err := s.kvClient.LoadWithPrefix(path.Join(Params.SegmentBinlogSubPath, prefix))
	if err != nil {
		return nil, err
	}

	for _, blob := range vs {
		m := &datapb.SegmentFieldBinlogMeta{}
		if err = proto.UnmarshalText(blob, m); err != nil {
			return nil, err
		}

		metas = append(metas, m)
	}
	return
}

func (s *Server) getDDLBinlogMeta(collID UniqueID) (metas []*datapb.DDLBinlogMeta, err error) {
	prefix, err := s.genKey(false, collID)
	if err != nil {
		return nil, err
	}

	_, vs, err := s.kvClient.LoadWithPrefix(path.Join(Params.CollectionBinlogSubPath, prefix))
	if err != nil {
		return nil, err
	}

	for _, blob := range vs {
		m := &datapb.DDLBinlogMeta{}
		if err = proto.UnmarshalText(blob, m); err != nil {
			return nil, err
		}

		metas = append(metas, m)
	}
	return
}

// prepareSegmentPos prepare segment flushed pos
func (s *Server) prepareSegmentPos(segmentID UniqueID, dmlPos, ddlPos *datapb.PositionPair) (map[string]string, error) {
	result := make(map[string]string, 2)
	if dmlPos != nil {
		key, err := s.genKey(false, segmentID)
		if err != nil {
			return nil, err
		}
		msPosPair := proto.MarshalTextString(dmlPos)
		result[path.Join(Params.SegmentDmlPosSubPath, key)] = msPosPair
	}
	if ddlPos != nil {
		key, err := s.genKey(false, segmentID)
		if err != nil {
			return nil, err
		}
		msPosPair := proto.MarshalTextString(ddlPos)
		result[path.Join(Params.SegmentDmlPosSubPath, key)] = msPosPair
	}

	return map[string]string{}, nil
}
