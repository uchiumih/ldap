package ldap

import (
	"fmt"
	"io"
	"log"
	"net"

	"github.com/egxp/ldap/ldaplib"
	ber "github.com/nmcclain/asn1-ber"
)

func (server *Server) handleConnectionAndSendBindDn(conn net.Conn) {
	boundDN := "" // "" == anonymous

	// Bind dn の bind request が送信済みか否かを表す真理値
	isSentBindDn := false

handler:
	for {
		// read incoming LDAP packet
		packet, err := ber.ReadPacket(conn)
		if err == io.EOF { // Client closed connection
			break
		} else if err != nil {
			log.Printf("handleConnection ber.ReadPacket ERROR: %s", err.Error())
			break
		}

		// sanity check this packet
		if len(packet.Children) < 2 {
			log.Print("len(packet.Children) < 2")
			break
		}
		// check the message ID and ClassType
		messageID, ok := packet.Children[0].Value.(uint64)
		if !ok {
			log.Print("malformed messageID")
			break
		}
		req := packet.Children[1]
		if req.ClassType != ber.ClassApplication {
			log.Print("req.ClassType != ber.ClassApplication")
			break
		}
		// handle controls if present
		controls := []Control{}
		if len(packet.Children) > 2 {
			for _, child := range packet.Children[2].Children {
				controls = append(controls, DecodeControl(child))
			}
		}

		//log.Printf("DEBUG: handling operation: %s [%d]", ApplicationMap[req.Tag], req.Tag)
		//ber.PrintPacket(packet) // DEBUG

		// dispatch the LDAP operation
		switch req.Tag { // ldap op code
		default:
			responsePacket := encodeLDAPResponse(messageID, ApplicationAddResponse, LDAPResultOperationsError, "Unsupported operation: add")
			if err = sendPacket(conn, responsePacket); err != nil {
				log.Printf("sendPacket error %s", err.Error())
			}
			log.Printf("Unhandled operation: %s [%d]", ApplicationMap[req.Tag], req.Tag)
			break handler

		case ApplicationBindRequest:
			server.Stats.countBinds(1)
			ldapResultCode := HandleBindRequest(req, server.BindFns, conn)
			if ldapResultCode == LDAPResultSuccess {
				boundDN, ok = req.Children[1].Value.(string)
				if !ok {
					log.Printf("Malformed Bind DN")
					break handler
				}
			}
			responsePacket := encodeBindResponse(messageID, ldapResultCode)
			if err = sendPacket(conn, responsePacket); err != nil {
				log.Printf("sendPacket error %s", err.Error())
				break handler
			}

			// Bind dn が空文字のとき、設定ファイルから bind dn を作成して bind request を送る
			bindDnIsEmpty := false
			judgeBindDnIsEmpty(packet, &bindDnIsEmpty)
			if bindDnIsEmpty {
				ok = server.sendBindDnRequest(conn, &boundDN)
				if !ok {
					break handler
				}
				isSentBindDn = true
			}
		case ApplicationSearchRequest:
			if !isSentBindDn {
				ok = server.sendBindDnRequest(conn, &boundDN)
				if !ok {
					break handler
				}
				isSentBindDn = true
			}

			server.Stats.countSearches(1)
			if err := HandleSearchRequest(req, &controls, messageID, boundDN, server, conn); err != nil {
				log.Printf("handleSearchRequest error %s", err.Error()) // TODO: make this more testable/better err handling - stop using log, stop using breaks?
				e := err.(*Error)
				if err = sendPacket(conn, encodeSearchDone(messageID, e.ResultCode)); err != nil {
					log.Printf("sendPacket error %s", err.Error())
					break handler
				}
				break handler
			} else {
				if err = sendPacket(conn, encodeSearchDone(messageID, LDAPResultSuccess)); err != nil {
					log.Printf("sendPacket error %s", err.Error())
					break handler
				}
			}
		case ApplicationUnbindRequest:
			server.Stats.countUnbinds(1)
			break handler // simply disconnect
		case ApplicationExtendedRequest:
			ldapResultCode := HandleExtendedRequest(req, boundDN, server.ExtendedFns, conn)
			responsePacket := encodeLDAPResponse(messageID, ApplicationExtendedResponse, ldapResultCode, LDAPResultCodeMap[ldapResultCode])
			if err = sendPacket(conn, responsePacket); err != nil {
				log.Printf("sendPacket error %s", err.Error())
				break handler
			}
		case ApplicationAbandonRequest:
			HandleAbandonRequest(req, boundDN, server.AbandonFns, conn)
			break handler

		case ApplicationAddRequest:
			ldapResultCode := HandleAddRequest(req, boundDN, server.AddFns, conn)
			responsePacket := encodeLDAPResponse(messageID, ApplicationAddResponse, ldapResultCode, LDAPResultCodeMap[ldapResultCode])
			if err = sendPacket(conn, responsePacket); err != nil {
				log.Printf("sendPacket error %s", err.Error())
				break handler
			}
		case ApplicationModifyRequest:
			ldapResultCode := HandleModifyRequest(req, boundDN, server.ModifyFns, conn)
			responsePacket := encodeLDAPResponse(messageID, ApplicationModifyResponse, ldapResultCode, LDAPResultCodeMap[ldapResultCode])
			if err = sendPacket(conn, responsePacket); err != nil {
				log.Printf("sendPacket error %s", err.Error())
				break handler
			}
		case ApplicationDelRequest:
			ldapResultCode := HandleDeleteRequest(req, boundDN, server.DeleteFns, conn)
			responsePacket := encodeLDAPResponse(messageID, ApplicationDelResponse, ldapResultCode, LDAPResultCodeMap[ldapResultCode])
			if err = sendPacket(conn, responsePacket); err != nil {
				log.Printf("sendPacket error %s", err.Error())
				break handler
			}
		case ApplicationModifyDNRequest:
			ldapResultCode := HandleModifyDNRequest(req, boundDN, server.ModifyDNFns, conn)
			responsePacket := encodeLDAPResponse(messageID, ApplicationModifyDNResponse, ldapResultCode, LDAPResultCodeMap[ldapResultCode])
			if err = sendPacket(conn, responsePacket); err != nil {
				log.Printf("sendPacket error %s", err.Error())
				break handler
			}
		case ApplicationCompareRequest:
			ldapResultCode := HandleCompareRequest(req, boundDN, server.CompareFns, conn)
			responsePacket := encodeLDAPResponse(messageID, ApplicationCompareResponse, ldapResultCode, LDAPResultCodeMap[ldapResultCode])
			if err = sendPacket(conn, responsePacket); err != nil {
				log.Printf("sendPacket error %s", err.Error())
				break handler
			}
		}
	}

	for _, c := range server.CloseFns {
		c.Close(boundDN, conn)
	}

	conn.Close()
}

func judgeBindDnIsEmpty(p *ber.Packet, result *bool) {
	class, tagtype, tag := ber.ClassMap[p.ClassType], ber.TypeMap[p.TagType], fmt.Sprintf("0x%02X", p.Tag)
	if p.ClassType == ber.ClassUniversal {
		tag = ber.TagMap[p.Tag]
	}

	if class == "Universal" &&
		tagtype == "Primative" &&
		tag == "Octet String" &&
		p.Data.Len() == 0 {
		*result = true
	}

	for _, child := range p.Children {
		judgeBindDnIsEmpty(child, result)
	}
}

func makeBindDnRequest(username, password string) *ber.Packet {
	var messageID uint64 = 0

	packet := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Request")
	packet.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, messageID, "MessageID"))
	bindRequest := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ApplicationBindRequest, nil, "Bind Request")
	bindRequest.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, 3, "Version"))
	bindRequest.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, username, "User Name"))
	bindRequest.AppendChild(ber.NewString(ber.ClassContext, ber.TypePrimitive, 0, password, "Password"))
	packet.AppendChild(bindRequest)

	// ber.PrintPacket(packet)	// DEBUG

	return packet
}

func (server *Server) sendBindDnRequest(conn net.Conn, boundDN *string) bool {
	var result bool

	bindDnPacket := makeBindDnRequest(ldaplib.BindDn, ldaplib.BindPass)
	bindDnReq := bindDnPacket.Children[1]

	// ber.PrintPacket(bindDnReq)	// DEBUG

	server.Stats.countBinds(1)
	ldapResultCode := HandleBindRequest(bindDnReq, server.BindFns, conn)
	if ldapResultCode == LDAPResultSuccess {
		*boundDN, result = bindDnReq.Children[1].Value.(string)
		if !result {
			log.Printf("Malformed Bind DN")
		}
	}
	messageID := bindDnPacket.Children[0].Value.(uint64)
	responsePacket := encodeBindResponse(messageID, ldapResultCode)
	if err := sendPacket(conn, responsePacket); err != nil {
		log.Printf("sendPacket error %s", err.Error())
		result = false
	}

	return result
}
