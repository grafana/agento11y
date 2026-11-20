package local

import "encoding/json"

func (s *Storage) queryConversationFiles() ([]conversationFile, error) {
	ready, err := s.sqliteReadsReady()
	if err != nil {
		return nil, err
	}
	return s.queryConversationFilesFrom(ready)
}

func (s *Storage) queryConversationFilesFrom(sqlReads bool) ([]conversationFile, error) {
	if !sqlReads {
		return s.conversationFiles()
	}
	var rows []sqlConversation
	if err := s.sql.db.Select("conv_id, activity, summary_revision").Order("activity DESC").Order("conv_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	files := make([]conversationFile, 0, len(rows))
	for _, row := range rows {
		files = append(files, conversationFile{sql: true, id: row.ConvID, modTime: row.Activity, revision: row.Revision})
	}
	s.sqlSummaries.prune(files)
	// SQL reads do not use the JSONL cache, so it only holds memory.
	s.summaries.prune(nil)
	return files, nil
}

func (s *Storage) conversationSummary(f conversationFile) (*fileSummary, error) {
	if !f.sql {
		return s.summaries.get(f)
	}
	return s.sqlSummaries.getWithDecoder(f, s.decodeSQLSummary)
}

func (s *Storage) decodeSQLSummary(f conversationFile) (*fileSummary, error) {
	return decodeConversationSummary(f, func(visit func(summaryRecord)) (int, error) {
		rows, err := s.sql.db.Model(&sqlGeneration{}).Select("received_at, raw, raw_codec").Where("conv_id = ?", f.id).Order("rowid ASC").Rows()
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		for rows.Next() {
			var receivedAt, codec string
			var stored []byte
			if err := rows.Scan(&receivedAt, &stored, &codec); err != nil {
				return 0, err
			}
			raw, err := decodeSQLRaw(codec, stored)
			if err != nil {
				return 0, err
			}
			var gen summaryGeneration
			if err := json.Unmarshal(raw, &gen); err != nil {
				var core summaryGenerationCore
				if err := json.Unmarshal(raw, &core); err != nil {
					return 0, err
				}
				gen = summaryGeneration{summaryGenerationCore: core}
			}
			visit(summaryRecord{ReceivedAt: receivedAt, ConversationID: f.id, Generation: gen})
		}
		return 0, rows.Err()
	})
}
