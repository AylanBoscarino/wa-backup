package storage

// Storage is the abstraction over where backup data is written.
// LocalStorage is the only implementation today; S3/GCS variants can be
// added later without touching the pipeline.
type Storage interface {
	// AppendMessage adiciona uma linha ao JSONL do grupo/mês.
	AppendMessage(groupSlug, yearMonth string, line []byte) error

	// WriteMedia salva o conteúdo de mídia e retorna o path relativo.
	// Se uma mídia com o mesmo hash já existir (de qualquer grupo/mês),
	// o byte slice é descartado e o path do arquivo original é retornado.
	WriteMedia(groupSlug, yearMonth, hash, ext string, data []byte) (relativePath string, err error)

	// Exists verifica se uma mídia com este hash já foi salva em qualquer
	// lugar do backup. Quando true, retorna o path relativo existente para
	// que mensagens novas possam referenciá-lo sem baixar de novo.
	Exists(hash, ext string) (exists bool, relativePath string, err error)

	// Close libera recursos (flush de buffers, fecha arquivos).
	Close() error
}
