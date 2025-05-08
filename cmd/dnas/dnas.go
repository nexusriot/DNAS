package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Transaction struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Amount float64 `json:"amount"`
}

type Block struct {
	Index        int
	Timestamp    string
	Transactions []Transaction
	PrevHash     string
	Hash         string
	Nonce        int
}

type Node struct {
	Port  string
	Peers []string
}

var (
	Blockchain []Block
	peers      []net.Conn
	peerLock   sync.Mutex
)

func calculateBlockHash(block Block) string {
	txData, _ := json.Marshal(block.Transactions)
	record := fmt.Sprintf("%d%s%s%s%d", block.Index, block.Timestamp, txData, block.PrevHash, block.Nonce)
	h := sha256.Sum256([]byte(record))
	return hex.EncodeToString(h[:])
}

func isBlockValid(newBlock, oldBlock Block) bool {
	if oldBlock.Index+1 != newBlock.Index {
		return false
	}
	if oldBlock.Hash != newBlock.PrevHash {
		return false
	}
	if calculateBlockHash(newBlock) != newBlock.Hash {
		return false
	}
	return true
}

func generateBlock(oldBlock Block, txs []Transaction) Block {
	newBlock := Block{
		Index:        oldBlock.Index + 1,
		Timestamp:    time.Now().String(),
		Transactions: txs,
		PrevHash:     oldBlock.Hash,
	}
	newBlock.Hash = calculateBlockHash(newBlock)
	return newBlock
}

func mineBlock(oldBlock Block, txs []Transaction, difficulty int) Block {
	newBlock := Block{
		Index:        oldBlock.Index + 1,
		Timestamp:    time.Now().String(),
		Transactions: txs,
		PrevHash:     oldBlock.Hash,
	}
	for {
		hash := calculateBlockHash(newBlock)
		if strings.HasPrefix(hash, strings.Repeat("0", difficulty)) {
			newBlock.Hash = hash
			break
		}
		newBlock.Nonce++
	}
	return newBlock
}

func calculateBalance(blockchain []Block, address string) float64 {
	var balance float64
	for _, block := range blockchain {
		for _, tx := range block.Transactions {
			if tx.From == address {
				balance -= tx.Amount
			}
			if tx.To == address {
				balance += tx.Amount
			}
		}
	}
	return balance
}

func broadcastBlock(block Block) {
	data, _ := json.Marshal(block)
	data = append(data, '\n')
	peerLock.Lock()
	defer peerLock.Unlock()
	for _, peer := range peers {
		_, _ = peer.Write(data)
	}
}

func handleConnection(conn net.Conn) {
	peerLock.Lock()
	peers = append(peers, conn)
	peerLock.Unlock()

	defer func() {
		peerLock.Lock()
		for i, p := range peers {
			if p == conn {
				peers = append(peers[:i], peers[i+1:]...)
				break
			}
		}
		peerLock.Unlock()
		conn.Close()
	}()

	data, _ := json.Marshal(Blockchain)
	conn.Write(append(data, '\n'))

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var newBlock Block
		if err := json.Unmarshal(scanner.Bytes(), &newBlock); err == nil {
			if isBlockValid(newBlock, Blockchain[len(Blockchain)-1]) {
				Blockchain = append(Blockchain, newBlock)
				broadcastBlock(newBlock)
				fmt.Println("New block received and broadcasted")
			}
		}
	}
}

func startServer(port string) {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Println("Error starting server:", err)
		return
	}
	fmt.Println("Listening on port", port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleConnection(conn)
	}
}

func (n *Node) ConnectToPeers() {
	for _, addr := range n.Peers {
		go func(address string) {
			conn, err := net.Dial("tcp", address)
			if err != nil {
				return
			}
			peerLock.Lock()
			peers = append(peers, conn)
			peerLock.Unlock()

			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				var newBlock Block
				if err := json.Unmarshal(scanner.Bytes(), &newBlock); err == nil {
					if isBlockValid(newBlock, Blockchain[len(Blockchain)-1]) {
						Blockchain = append(Blockchain, newBlock)
						broadcastBlock(newBlock)
						fmt.Println("Synced block from", address)
					}
				}
			}
		}(addr)
	}
}

func saveBlockchainToFile(filename string) error {
	data, err := json.MarshalIndent(Blockchain, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0644)
}

func loadBlockchainFromFile(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &Blockchain)
}

func startAPI(port string) {
	http.HandleFunc("/chain", func(w http.ResponseWriter, r *http.Request) {
		data, _ := json.MarshalIndent(Blockchain, "", "  ")
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	http.HandleFunc("/balance/", func(w http.ResponseWriter, r *http.Request) {
		addr := strings.TrimPrefix(r.URL.Path, "/balance/")
		balance := calculateBalance(Blockchain, addr)
		json.NewEncoder(w).Encode(map[string]any{"address": addr, "balance": balance})
	})

	fmt.Println("API listening on http://localhost:" + port)
	http.ListenAndServe(":"+port, nil)
}

func main() {
	const dbFile = "blockchain.json"
	if err := loadBlockchainFromFile(dbFile); err != nil {
		genesis := Block{Index: 0, Timestamp: time.Now().String(), Hash: "GENESIS"}
		genesis.Hash = calculateBlockHash(genesis)
		Blockchain = append(Blockchain, genesis)
	}

	node := Node{
		Port:  "3001",
		Peers: []string{"localhost:3000"},
	}
	go startServer(node.Port)
	node.ConnectToPeers()
	go startAPI("8080")

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Enter sender,receiver,amount: ")
		line, _ := reader.ReadString('\n')
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) != 3 {
			continue
		}
		amount := 0.0
		fmt.Sscanf(parts[2], "%f", &amount)
		tx := Transaction{From: parts[0], To: parts[1], Amount: amount}
		newBlock := mineBlock(Blockchain[len(Blockchain)-1], []Transaction{tx}, 3)
		if isBlockValid(newBlock, Blockchain[len(Blockchain)-1]) {
			Blockchain = append(Blockchain, newBlock)
			broadcastBlock(newBlock)
			_ = saveBlockchainToFile(dbFile)
			fmt.Println("Block mined and broadcasted")
		}
	}
}
