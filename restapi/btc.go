package main
 
import (
	"bytes"
	"encoding/hex"
	"strconv"
	"time"
	"sort"
	"fmt"
	"log"
	"io/ioutil"
	"path/filepath"
 
	"github.com/conformal/btcjson"
	"github.com/conformal/btcnet"
	"github.com/conformal/btcscript"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
	"github.com/conformal/btcrpcclient"

	"github.com/FactomProject/FactomCode/notaryapi"
	
)

var fee btcutil.Amount
var wif *btcutil.WIF

// ByAmount defines the methods needed to satisify sort.Interface to
// sort a slice of Utxos by their amount.
type ByAmount []btcjson.ListUnspentResult

func (u ByAmount) Len() int           { return len(u) }
func (u ByAmount) Less(i, j int) bool { return u[i].Amount < u[j].Amount }
func (u ByAmount) Swap(i, j int)      { u[i], u[j] = u[j], u[i] }


func SendRawTransactionToBTC(hash []byte) (*btcwire.ShaHash, error) {
	
	msgtx, err := createRawTransaction(hash)
	if err != nil {
		return nil, fmt.Errorf("cannot create Raw Transaction: %s", err)
	}
	
	shaHash, err := sendRawTransaction(msgtx)
	if err != nil {
		return nil, fmt.Errorf("cannot send Raw Transaction: %s", err)
	}
	
	return shaHash, nil
}


func initWallet(addrStr string) error {
	
	fee, _ = btcutil.NewAmount(btcTransFee)
	
	err := client.WalletPassphrase(walletPassphrase, int64(2))
	if err != nil {
		return fmt.Errorf("cannot unlock wallet with passphrase: %s", err)
	}	

	currentAddr, err = btcutil.DecodeAddress(addrStr, &btcnet.TestNet3Params)
	if err != nil {
		return fmt.Errorf("cannot decode address: %s", err)
	}

	wif, err = client.DumpPrivKey(currentAddr) 
	if err != nil { 
		return fmt.Errorf("cannot get WIF: %s", err)
	}

	return nil
	
}


func createRawTransaction(hash []byte) (*btcwire.MsgTx, error) {
	
	msgtx := btcwire.NewMsgTx()

	minconf := 0	// 1
	maxconf := 999999
	
	addrs := []btcutil.Address{currentAddr}
		
	unspent, err := client.ListUnspentMinMaxAddresses(minconf, maxconf, addrs)
	if err != nil {
		return nil, fmt.Errorf("cannot ListUnspentMinMaxAddresses: %s", err)
	}
	fmt.Printf("unspent, len=%d", len(unspent))

	// Sort eligible inputs, as unspent expects these to be sorted
	// by amount in reverse order.
	sort.Sort(sort.Reverse(ByAmount(unspent)))
	
	inputs, btcin, err := selectInputs(unspent, minconf)
	if err != nil {
		return nil, fmt.Errorf("cannot selectInputs: %s", err)
	}
	fmt.Println("selectedInputs, len=%d", len(inputs))
	
	change := btcin - fee
	if err = addTxOuts(msgtx, change, hash); err != nil {
		return nil, fmt.Errorf("cannot addTxOuts: %s", err)
	}

	if err = addTxIn(msgtx, inputs); err != nil {
		return nil, fmt.Errorf("cannot addTxIn: %s", err)
	}

	if err = validateMsgTx(msgtx, inputs); err != nil {
		return nil, fmt.Errorf("cannot validateMsgTx: %s", err)
	}

	return msgtx, nil
}


// For every unspent output given, add a new input to the given MsgTx. Only P2PKH outputs are
// supported at this point.
func addTxIn(msgtx *btcwire.MsgTx, outputs []btcjson.ListUnspentResult) error {
	
	for _, output := range outputs {
		fmt.Printf("unspentResult: %#v", output)
		prevTxHash, err := btcwire.NewShaHashFromStr(output.TxId)
		if err != nil {
			return fmt.Errorf("cannot get sha hash from str: %s", err)
		}
		
		outPoint := btcwire.NewOutPoint(prevTxHash, output.Vout)
		msgtx.AddTxIn(btcwire.NewTxIn(outPoint, nil))
	}

	for i, output := range outputs {
	 
		subscript, err := hex.DecodeString(output.ScriptPubKey)
		if err != nil {
			return fmt.Errorf("cannot decode scriptPubKey: %s", err)
		}
		
		fmt.Println("subscript ", string(subscript))
	 
		sigScript, err := btcscript.SignatureScript(msgtx, i, subscript,
			btcscript.SigHashAll, wif.PrivKey.ToECDSA(), true)
		if err != nil {
			return fmt.Errorf("cannot create scriptSig: %s", err)
		}
		msgtx.TxIn[i].SignatureScript = sigScript
		
		fmt.Println("sigScript ", string(sigScript))
		
	}
	return nil
}

func addTxOuts(msgtx *btcwire.MsgTx, change btcutil.Amount, hash []byte) error {
 
 	header := []byte{0x46, 0x61, 0x63, 0x74, 0x6f, 0x6d, 0x21, 0x21}	// Factom!!
	hash = append(header, hash...)

	
	builder := btcscript.NewScriptBuilder()
	builder.AddOp(btcscript.OP_RETURN)
	builder.AddData(hash)
	opReturn := builder.Script()
	msgtx.AddTxOut(btcwire.NewTxOut(0, opReturn))

	// Check if there are leftover unspent outputs, and return coins back to
	// a new address we own.
	if change > 0 {

		// Spend change.
		pkScript, err := btcscript.PayToAddrScript(currentAddr)
		if err != nil {
			return fmt.Errorf("cannot create txout script: %s", err)
		}
		//btcscript.JSONToAmount(jsonAmount float64) (int64)
		msgtx.AddTxOut(btcwire.NewTxOut(int64(change), pkScript))
	}
	return nil
}



// selectInputs selects the minimum number possible of unspent
// outputs to use to create a new transaction that spends amt satoshis.
// btcout is the total number of satoshis which would be spent by the
// combination of all selected previous outputs.  err will equal
// ErrInsufficientFunds if there are not enough unspent outputs to spend amt
// amt.
func selectInputs(eligible []btcjson.ListUnspentResult, minconf int) (selected []btcjson.ListUnspentResult, out btcutil.Amount, err error) {
	// Iterate throguh eligible transactions, appending to outputs and
	// increasing out.  This is finished when out is greater than the
	// requested amt to spend.
	selected = make([]btcjson.ListUnspentResult, 0, len(eligible))
	for _, e := range eligible {
		amount, err := btcutil.NewAmount(e.Amount)
		if err != nil {
			fmt.Println("err in creating NewAmount")
			continue
		}
		selected = append(selected, e)
		out += amount
		if out >= fee {
			return selected, out, nil
		}
	}
	if out < fee {
		return nil, 0, fmt.Errorf("insufficient funds: transaction requires %v fee, but only %v spendable", fee, out)		 
	}

	return selected, out, nil
}


func validateMsgTx(msgtx *btcwire.MsgTx, inputs []btcjson.ListUnspentResult) error {
	flags := btcscript.ScriptCanonicalSignatures | btcscript.ScriptStrictMultiSig
	bip16 := time.Now().After(btcscript.Bip16Activation)
	if bip16 {
		flags |= btcscript.ScriptBip16
	}
	for i, txin := range msgtx.TxIn {
	 
		subscript, err := hex.DecodeString(inputs[i].ScriptPubKey)
		if err != nil {
			return fmt.Errorf("cannot decode scriptPubKey: %s", err)
		}

		engine, err := btcscript.NewScript(
			txin.SignatureScript, subscript, i, msgtx, flags)
		if err != nil {
			return fmt.Errorf("cannot create script engine: %s", err)
		}
		if err = engine.Execute(); err != nil {
			return fmt.Errorf("cannot validate transaction: %s", err)
		}
	}
	return nil
}


func sendRawTransaction(msgtx *btcwire.MsgTx) (*btcwire.ShaHash, error) {

	buf := bytes.Buffer{}
	buf.Grow(msgtx.SerializeSize())
	if err := msgtx.BtcEncode(&buf, btcwire.ProtocolVersion); err != nil {
		// Hitting OOM by growing or writing to a bytes.Buffer already
		// panics, and all returned errors are unexpected.
		//panic(err) //?? should we have retry logic?
		return nil, err
	}
	
	txRawResult, err := client.DecodeRawTransaction(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("cannot Decode Raw Transaction: %s", err)
	}
	fmt.Println("txRawResult: ", txRawResult)
	
	shaHash, err := client.SendRawTransaction(msgtx, false)
	if err != nil {
		return nil, fmt.Errorf("cannot send Raw Transaction: %s", err)
	}
	fmt.Println("btc txHash: ", shaHash)	// new tx hash
	
	return shaHash, nil
}



func initRPCClient() error {
	// Only override the handlers for notifications you care about.
	// Also note most of the handlers will only be called if you register
	// for notifications.  See the documentation of the btcrpcclient
	// NotificationHandlers type for more details about each handler.
	ntfnHandlers := btcrpcclient.NotificationHandlers{
		OnAccountBalance: func(account string, balance btcutil.Amount, confirmed bool) {
		     //go newBalance(account, balance, confirmed)
		     fmt.Println("OnAccountBalance, account=", account, ", balance=", balance.String, ", confirmed=", confirmed)
	    },
		OnBlockConnected: func(hash *btcwire.ShaHash, height int32) {
			fmt.Println("OnBlockConnected")
			//go newBlock(hash, height)	// no need
		},
	}
	 
	// Connect to local btcwallet RPC server using websockets.
	certHomeDir := btcutil.AppDataDir(certHomePath, false)
	certs, err := ioutil.ReadFile(filepath.Join(certHomeDir, "rpc.cert"))
	if err != nil {
		return fmt.Errorf("cannot read rpc.cert file: %s", err)
	}
	connCfg := &btcrpcclient.ConnConfig{
		Host:         rpcClientHost,
		Endpoint:     rpcClientEndpoint,
		User:         rpcClientUser,
		Pass:         rpcClientPass,
		Certificates: certs,
	}
	
	client, err = btcrpcclient.New(connCfg, &ntfnHandlers)	
	if err != nil {
		return fmt.Errorf("cannot create rpc client: %s", err)
	}
	
	return nil
}


func shutdown(client *btcrpcclient.Client) {
	// For this example gracefully shutdown the client after 10 seconds.
	// Ordinarily when to shutdown the client is highly application
	// specific.
	log.Println("Client shutdown in 2 seconds...")
	time.AfterFunc(time.Second*2, func() {
		log.Println("Going down...")
		client.Shutdown()
	})
	defer log.Println("Shutdown done!")
	// Wait until the client either shuts down gracefully (or the user
	// terminates the process with Ctrl+C).
	client.WaitForShutdown()
}


var waiting bool = false


func newEntryBlock(chain *notaryapi.Chain) (*notaryapi.Block, *notaryapi.Hash){

	// acquire the last block
	block := chain.Blocks[len(chain.Blocks)-1]

 	if len(block.EBEntries) < 1{
 		//log.Println("No new entry found. No block created for chain: "  + notaryapi.EncodeChainID(chain.ChainID))
 		return nil, nil
 	}

	// Create the block and add a new block for new coming entries
	chain.BlockMutex.Lock()
	blkhash, _ := notaryapi.CreateHash(block)
	block.IsSealed = true	
	chain.NextBlockID++	
	newblock, _ := notaryapi.CreateBlock(chain, block, 10)
	chain.Blocks = append(chain.Blocks, newblock)
	chain.BlockMutex.Unlock()
    
    //Store the block in db
	db.ProcessEBlockBatch(blkhash, block)	
	log.Println("block" + strconv.FormatUint(block.Header.BlockID, 10) +" created for chain: "  + chain.ChainID.String())	
	
	return block, blkhash
}


func newFactomBlock(chain *notaryapi.FChain) {

	// acquire the last block
	block := chain.Blocks[len(chain.Blocks)-1]

 	if len(block.FBEntries) < 1{
 		//log.Println("No Factom block created for chain ... because no new entry is found.")
 		return
 	} 
	
	// Create the block add a new block for new coming entries
	chain.BlockMutex.Lock()
	blkhash, _ := notaryapi.CreateHash(block)
	block.IsSealed = true	
	chain.NextBlockID++
	newblock, _ := notaryapi.CreateFBlock(chain, block, 10)
	chain.Blocks = append(chain.Blocks, newblock)
	chain.BlockMutex.Unlock()

	//Store the block in db
	db.ProcessFBlockBatch(blkhash, block) 	
	//need to add a FB process queue in db??	
	log.Println("block" + strconv.FormatUint(block.Header.BlockID, 10) +" created for factom chain: "  + notaryapi.EncodeBinary(chain.ChainID))
	
	//Send transaction to BTC network
	txHash, err := SendRawTransactionToBTC(blkhash.Bytes)
	if err != nil {
		log.Fatalf("cannot init rpc client: %s", err)
	}
	
	// Create a FBInfo and insert it into db
	fbInfo := new (notaryapi.FBInfo)
	fbInfo.FBHash = blkhash
	btcTxHash := new (notaryapi.Hash)
	btcTxHash.Bytes = txHash.Bytes()
	fbInfo.BTCTxHash = btcTxHash
	fbInfo.FBlockID = block.Header.BlockID
	
	db.InsertFBInfo(blkhash, fbInfo)
	
	// Export all db records associated w/ this new factom block
	ExportDbToFile(blkhash)
	
    log.Print("Recorded ", blkhash.Bytes, " in BTC transaction hash:\n",txHash)
    
    
}