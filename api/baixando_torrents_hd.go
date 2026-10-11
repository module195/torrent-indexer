package indexers

import (
    "net/http"
    "github.com/felipemarinho97/torrent-indexer/utils"
)

var baixando_torrents_hd = IndexerMeta{
    Label:       "baixando_torrents_hd",
    URL:         utils.GetIndexerURLFromEnv("INDEXER_BAIXANDO_TORRENTS_HD_URL", "https://baixandotorrentshd.org/"),
    SearchURL:   "?s=",          // ajuste se for diferente
    PagePattern: "page/%s/",
}

func (i *Indexer) HandlerBaixandoTorrentsHDIndexer(w http.ResponseWriter, r *http.Request) {
    // Copie a lógica do HandlerRedeTorrentIndexer
    // e altere os seletores CSS conforme o site
}
