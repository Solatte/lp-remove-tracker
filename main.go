package main

import (
	"errors"
	"log"
	"math/big"

	"github.com/gagliardetto/solana-go"
	"github.com/iqbalbaharum/go-solana-mev-bot/internal/adapter"
	"github.com/iqbalbaharum/go-solana-mev-bot/internal/coder"
	"github.com/iqbalbaharum/go-solana-mev-bot/internal/config"
	"github.com/iqbalbaharum/go-solana-mev-bot/internal/generators"
	instructions "github.com/iqbalbaharum/go-solana-mev-bot/internal/instructions"
	bot "github.com/iqbalbaharum/go-solana-mev-bot/internal/library"
	"github.com/iqbalbaharum/go-solana-mev-bot/internal/liquidity"
	"github.com/iqbalbaharum/go-solana-mev-bot/internal/rpc"
	"github.com/iqbalbaharum/go-solana-mev-bot/internal/types"
	pb "github.com/rpcpool/yellowstone-grpc/examples/golang/proto"
)

func loadAdapter() {
	adapter.GetRedisClient(0)
}

var (
	client           *pb.GeyserClient
	latestBlockhash  string
	wsolTokenAccount solana.PublicKey
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	err := config.InitEnv()
	if err != nil {
		return
	}

	ata, err := getOrCreateAssociatedTokenAccount()
	if err != nil {
		log.Print(err)
		return
	}

	log.Printf("WSOL Associated Token Account %s", ata)
	wsolTokenAccount = *ata

	generators.GrpcConnect(config.GrpcAddr, config.InsecureConnection)

	txChannel := make(chan generators.GeyserResponse)

	go func() {
		for response := range txChannel {
			processResponse(response)
		}
	}()

	generators.GrpcSubscribeByAddresses(
		config.GrpcToken,
		[]string{config.RAYDIUM_AMM_V4.String()},
		[]string{}, txChannel)

	defer func() {
		if err := generators.CloseConnection(); err != nil {
			log.Printf("Error closing gRPC connection: %v", err)
		}
	}()
}

func processResponse(response generators.GeyserResponse) {
	// Your processing logic here
	latestBlockhash = response.MempoolTxns.RecentBlockhash

	c := coder.NewRaydiumAmmInstructionCoder()
	for _, ins := range response.MempoolTxns.Instructions {
		programId := response.MempoolTxns.AccountKeys[ins.ProgramIdIndex]

		if programId == config.RAYDIUM_AMM_V4.String() {
			decodedIx, err := c.Decode(ins.Data)
			if err != nil {
				// log.Println("Failed to decode instruction:", err)
				continue
			}

			switch decodedIx.(type) {
			case coder.Initialize2:
			case coder.Withdraw:
				log.Println("Withdraw", response.MempoolTxns.Signature)
				processWithdraw(ins, response)
			case coder.SwapBaseIn:
				processSwapBaseIn(ins, response)
			case coder.SwapBaseOut:
			default:
				log.Println("Unknown instruction type")
			}
		}
	}
}

func getPublicKeyFromTx(pos int, tx generators.MempoolTxn, instruction generators.TxInstruction) (*solana.PublicKey, error) {
	accountIndexes := instruction.Accounts
	if len(accountIndexes) == 0 {
		return nil, errors.New("no account indexes provided")
	}

	lookupsForAccountKeyIndex := bot.GenerateTableLookup(tx.AddressTableLookups)
	var ammId *solana.PublicKey
	accountIndex := int(accountIndexes[pos])

	if accountIndex >= len(tx.AccountKeys) {
		lookupIndex := accountIndex - len(tx.AccountKeys)
		lookup := lookupsForAccountKeyIndex[lookupIndex]
		table, err := bot.GetLookupTable(solana.MustPublicKeyFromBase58(lookup.LookupTableKey))
		if err != nil {
			return nil, err
		}

		if int(lookup.LookupTableIndex) >= len(table.Addresses) {
			return nil, errors.New("lookup table index out of range")
		}

		ammId = &table.Addresses[lookup.LookupTableIndex]

	} else {
		key := solana.MustPublicKeyFromBase58(tx.AccountKeys[accountIndex])
		ammId = &key
	}

	return ammId, nil
}

func processWithdraw(ins generators.TxInstruction, tx generators.GeyserResponse) {
	ammId, err := getPublicKeyFromTx(1, tx.MempoolTxns, ins)
	if err != nil {
		return
	}

	if ammId == nil {
		log.Print("Unable to retrieve AMM ID")
		return
	}

	pKey, err := liquidity.GetPoolKeys(ammId)
	if err != nil {
		log.Printf("%s | %s", ammId, err)
		return
	}

	reserve, err := liquidity.GetPoolSolBalance(pKey)
	if err != nil {
		log.Printf("%s | %s", ammId, err)
		return
	}

	if reserve > uint64(config.LAMPORTS_PER_SOL) {
		log.Printf("%s | Pool still have high balance", ammId)
		return
	}

	blockhash, err := solana.HashFromBase58(latestBlockhash)

	if err != nil {
		log.Print(err)
		return
	}

	compute := instructions.ComputeUnit{
		MicroLamports: 1000000,
		Units:         85000,
		Tip:           0,
	}

	options := instructions.TxOption{
		Blockhash: blockhash,
	}

	signatures, transaction, err := instructions.MakeSwapInstructions(
		pKey,
		wsolTokenAccount,
		compute,
		options,
		1000000,
		0,
		"buy",
		"rpc",
	)

	if err != nil {
		log.Print(err)
		return
	}

	log.Printf("%s | BUY | %s", ammId, signatures)

	rpc.SendTransaction(transaction)
}

/**
* Process swap base in instruction
 */
func processSwapBaseIn(ins generators.TxInstruction, tx generators.GeyserResponse) {
	var ammId *solana.PublicKey
	var openbookId *solana.PublicKey
	var sourceTokenAccount *solana.PublicKey
	var destinationTokenAccount *solana.PublicKey
	var signerPublicKey *solana.PublicKey

	var err error
	ammId, err = getPublicKeyFromTx(1, tx.MempoolTxns, ins)
	if err != nil {
		return
	}

	if ammId == nil {
		return
	}

	openbookId, err = getPublicKeyFromTx(7, tx.MempoolTxns, ins)
	if err != nil {
		return
	}

	var sourceAccountIndex int
	var destinationAccountIndex int
	var signerAccountIndex int

	if openbookId.String() == config.OPENBOOK_ID.String() {
		sourceAccountIndex = 15
		destinationAccountIndex = 16
		signerAccountIndex = 17
	} else {
		sourceAccountIndex = 14
		destinationAccountIndex = 15
		signerAccountIndex = 16
	}

	sourceTokenAccount, err = getPublicKeyFromTx(sourceAccountIndex, tx.MempoolTxns, ins)
	destinationTokenAccount, err = getPublicKeyFromTx(destinationAccountIndex, tx.MempoolTxns, ins)
	signerPublicKey, err = getPublicKeyFromTx(signerAccountIndex, tx.MempoolTxns, ins)

	if sourceTokenAccount == nil || destinationTokenAccount == nil || signerPublicKey == nil {
		return
	}

	if !signerPublicKey.Equals(config.Payer.PublicKey()) {
		isTracked, err := bot.GetAmmTrackingStatus(ammId)
		if err != nil {
			log.Print(err)
			return
		}

		if !isTracked {
			return
		}

	}

	pKey, err := liquidity.GetPoolKeys(ammId)
	if err != nil {
		return
	}

	mint, _, err := liquidity.GetMint(pKey)
	if err != nil {
		return
	}

	amount := bot.GetBalanceFromTransaction(tx.MempoolTxns.PreTokenBalances, tx.MempoolTxns.PostTokenBalances, mint)
	amountSol := bot.GetBalanceFromTransaction(tx.MempoolTxns.PreTokenBalances, tx.MempoolTxns.PostTokenBalances, config.WRAPPED_SOL)

	if signerPublicKey.Equals(config.Payer.PublicKey()) {
		chunk, err := bot.GetTokenChunk(ammId)
		if err != nil {
			if err.Error() == "key not found" {
				bot.SetTokenChunk(ammId, types.TokenChunk{
					Total:     amount,
					Remaining: amount,
					Chunk:     new(big.Int).Div(amount, big.NewInt(10)),
				})

				bot.RegisterAmm(ammId)
			}
			return
		}

		if chunk.Remaining.Uint64() == 0 {
			bot.UnregisterAmm(ammId)
			log.Printf("%s | No more chunk remaining ", ammId)
			return
		} else {
			chunk.Remaining = new(big.Int).Sub(chunk.Remaining, amount)
			bot.SetTokenChunk(ammId, chunk)
		}

		return
	}

	// Only proceed if the amount is greater than 0.011 SOL and amount of SOL is a negative number (represent buy action)
	log.Printf("%s | %d | %s | %s", ammId, amount.Sign(), amountSol, tx.MempoolTxns.Signature)
	if amount.Sign() == -1 && amountSol.Cmp(big.NewInt(1000000)) == 1 {
		log.Printf("%s | Potential entry %d SOL | %s", ammId, amountSol, tx.MempoolTxns.Signature)

		blockhash, err := solana.HashFromBase58(latestBlockhash)

		if err != nil {
			log.Print(err)
			return
		}

		var tip uint64
		var minAmountOut uint64
		var useStakedRPCFlag bool = false
		if amountSol.Uint64() > 200000000 {
			tip = 200000000
			minAmountOut = 200000000
			useStakedRPCFlag = true
		} else {
			tip = 0
			minAmountOut = 1000000
			useStakedRPCFlag = false
		}

		compute := instructions.ComputeUnit{
			MicroLamports: 10000000,
			Units:         60000,
			Tip:           tip,
		}

		options := instructions.TxOption{
			Blockhash: blockhash,
		}

		chunk, err := bot.GetTokenChunk(ammId)
		if err != nil {
			log.Printf("%s | %s", ammId, err)
			return
		}

		if (chunk.Remaining).Uint64() == 0 {
			log.Printf("%s | No more chunk remaining", ammId)
			return
		}

		signatures, transaction, err := instructions.MakeSwapInstructions(
			pKey,
			wsolTokenAccount,
			compute,
			options,
			chunk.Chunk.Uint64(),
			minAmountOut,
			"sell",
			"bloxroute",
		)

		if err != nil {
			log.Printf("%s | %s", ammId, err)
			return
		}

		rpc.SubmitBloxRouteTransaction(transaction, useStakedRPCFlag)

		log.Printf("%s | SELL | %s", ammId, signatures)
	}
}

func getOrCreateAssociatedTokenAccount() (*solana.PublicKey, error) {

	ata, tx, err := instructions.ValidatedAssociatedTokenAccount(&config.WRAPPED_SOL)
	if err != nil {
		return nil, err
	}

	if tx != nil {
		log.Print("Creating WSOL associated token account")
		rpc.SendTransaction(tx)
	}

	return &ata, nil
}
