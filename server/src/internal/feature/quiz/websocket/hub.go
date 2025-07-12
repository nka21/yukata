// server/src/internal/feature/quiz/websocket/hub.go
package websocket

import (
	"encoding/json"
	"log"
	"server/src/internal/database"
	"server/src/internal/feature/quiz/types"
	"sync"
)

// MessageProcessor はクライアントからのメッセージを処理する責務を持つインターフェースです。
type MessageProcessor interface {
	ProcessClientMessage(roomID, userID string, message []byte)
}

type RoomHub struct {
	rooms      map[string]map[*Client]bool
	mu         sync.RWMutex
	Register   chan *Client
	Unregister chan *Client
	Broadcast  chan *types.Message
	Inbound    chan *InboundMessage
	Processor  MessageProcessor
	DBHandler  *database.DBHandler
}

func NewRoomHub(db *database.DBHandler) *RoomHub {
	return &RoomHub{
		rooms:      make(map[string]map[*Client]bool),
		Register:   make(chan *Client),
		Unregister: make(chan *Client),
		Broadcast:  make(chan *types.Message),
		Inbound:    make(chan *InboundMessage),
		DBHandler:  db,
	}
}

func (h *RoomHub) Run() {
	for {
		select {
		case client := <-h.Register:
			h.registerClient(client)
		case client := <-h.Unregister:
			h.unregisterClient(client)
		case message := <-h.Broadcast:
			h.broadcastMessage(message)
		case inboundMessage := <-h.Inbound:
			if h.Processor != nil {
				client := inboundMessage.Client
				h.Processor.ProcessClientMessage(client.RoomID, client.UserID, inboundMessage.Message)
			}
		}
	}
}

func (h *RoomHub) registerClient(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	roomID := client.RoomID
	if _, ok := h.rooms[roomID]; !ok {
		h.rooms[roomID] = make(map[*Client]bool)
	}
	h.rooms[roomID][client] = true
	log.Printf("Client %s registered to room %s", client.UserID, roomID)
	joinMsg := &types.Message{
		Type:    "user_joined",
		Payload: map[string]string{"userId": client.UserID},
		RoomID:  roomID,
	}
	// このメソッドはRun goroutineから呼ばれるため、直接broadcastMessageを呼ぶとデッドロックの可能性がある
	// Broadcastチャネルに送信するのが安全
	go func() {
		h.Broadcast <- joinMsg
	}()
}

func (h *RoomHub) unregisterClient(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	roomID := client.RoomID
	userID := client.UserID

	db, err := h.DBHandler.ReadDB()
	if err != nil {
		log.Printf("error: failed to read db: %v", err)
		return
	}

	roomData, roomExists := db.Rooms[roomID]

	var isHost bool = false
	if roomExists {
		hostPlayer, hostPlayerExists := roomData.Players[roomData.HostID]
		if hostPlayerExists && hostPlayer.Name == userID {
			isHost = true
		}
	}

	if room, ok := h.rooms[roomID]; ok {
		if _, ok := room[client]; ok {
			delete(h.rooms[roomID], client)
			close(client.Send)
			log.Printf("Client %s unregistered from room %s", userID, roomID)

			if isHost {
				// --- ホストが退出した場合の処理 ---
				log.Printf("Host %s has left. Closing room %s.", userID, roomID)

				// ルーム解散メッセージを全員に送信
				closeMsg := &types.Message{
					Type:    "room_closed",
					Payload: map[string]string{"message": "ホストが退出したため、ルームは解散されました。"},
					RoomID:  roomID,
				}

				// broadcastMessageはロックを取得するため、デッドロックを避けるためゴルーチンで実行
				go h.broadcastMessage(closeMsg)

				for otherClient := range room {
					if otherClient != client { // 自分自身（ホスト）は除く
						close(otherClient.Send) // 各クライアントのSendチャネルを閉じると、WritePumpが終了し接続が切れる
					}
				}
				// DBからルームを削除
				delete(db.Rooms, roomID)
				if err := h.DBHandler.WriteDB(db); err != nil {
					log.Printf("error: failed to write DB after deleting room: %v", err)
				}

				// サーバー側のルーム情報を削除
				delete(h.rooms, roomID)

			} else if len(h.rooms[roomID]) == 0 {
				// --- 最後のユーザーが退出した場合の処理（ホスト以外） ---
				delete(h.rooms, roomID)
				log.Printf("Room %s closed", roomID)
			} else {
				// --- 通常のユーザーが退出した場合の処理 ---
				leaveMsg := &types.Message{
					Type:    "user_left",
					Payload: map[string]string{"userId": userID},
					RoomID:  roomID,
				}
				go h.broadcastMessage(leaveMsg)
			}
		}
	}
}

func (h *RoomHub) broadcastMessage(message *types.Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	roomID := message.RoomID
	if room, ok := h.rooms[roomID]; ok {
		jsonMsg, err := json.Marshal(message)
		if err != nil {
			log.Printf("error: failed to marshal broadcast message: %v", err)
			return
		}

		for client := range room {
			select {
			case client.Send <- jsonMsg:
			default:
				// 送信に失敗した場合（チャネルがブロックされている）、クライアントを切断
				log.Printf("Client %s send buffer is full. Unregistering.", client.UserID)
				// ★★★ ここが修正点 ★★★
				// 関数呼び出しではなくチャネルに送信する
				h.Unregister <- client

			}
		}
	}
}

func (h *RoomHub) GetClientIDs(roomID string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var userIDs []string
	if room, ok := h.rooms[roomID]; ok {
		for client := range room {
			userIDs = append(userIDs, client.UserID)
		}
	}
	return userIDs
}
