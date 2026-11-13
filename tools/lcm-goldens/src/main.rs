//! Usage: lcm-goldens --network-passphrase <passphrase> <file>...
//!
//! Each file is either a stellar-core golden (`*.xdr`: LedgerCloseMeta records in
//! RFC 5531 record-marking framing, as written by `stellar-core test --capture-lcm`)
//! or a corpus file (`*.b64`: one base64 LedgerCloseMeta). Every record is decoded
//! with rs-stellar-xdr, re-encoded, and compared byte for byte.
//!
//! Prints one JSON line per record:
//!   {"file":..,"record":i,"decode":"ok","roundTripIdentical":bool,
//!    "txSet":[{"hash":<hex>,"envelope":<xdr-json>}..],"value":<xdr-json>}
//! or {"file":..,"record":i,"decode":"error","error":".."}.
//! `txSet` lists every envelope of the ledger's transaction set with its hash under
//! the given passphrase, so a caller can pair envelopes with `tx_processing` results.
//! Exit code 0 only if every record of every file decoded and re-encoded identically.
use sha2::{Digest, Sha256};
use std::{env, fs, io::Write, process::ExitCode};
use stellar_xdr::{
    GeneralizedTransactionSet, LedgerCloseMeta, Limits, ReadXdr, TransactionEnvelope,
    TransactionPhase, TxSetComponent, WriteXdr,
};

const LAST_FRAGMENT: u32 = 0x8000_0000;

/// Splits a record-marked stream into records. Only single-fragment records are
/// accepted, which is what stellar-core's XDROutputFileStream writes.
fn split_frames(bytes: &[u8]) -> Result<Vec<&[u8]>, String> {
    let mut out = Vec::new();
    let mut pos = 0usize;
    while pos < bytes.len() {
        if bytes.len() - pos < 4 {
            return Err(format!("truncated record mark at byte {pos}"));
        }
        let mark = u32::from_be_bytes(bytes[pos..pos + 4].try_into().unwrap());
        if mark & LAST_FRAGMENT == 0 {
            return Err(format!("multi-fragment record at byte {pos} is not supported"));
        }
        let len = (mark & !LAST_FRAGMENT) as usize;
        let start = pos + 4;
        let end = start + len;
        if end > bytes.len() {
            return Err(format!("record at byte {pos} claims {len} bytes, file ends first"));
        }
        out.push(&bytes[start..end]);
        pos = end;
    }
    Ok(out)
}

fn records(path: &str) -> Result<Vec<Vec<u8>>, String> {
    if path.ends_with(".b64") {
        let text = fs::read_to_string(path).map_err(|e| e.to_string())?;
        let value = LedgerCloseMeta::from_xdr_base64(text.trim(), Limits::none())
            .map_err(|e| format!("base64/XDR: {e}"))?;
        let again = value.to_xdr_base64(Limits::none()).map_err(|e| e.to_string())?;
        if again != text.trim() {
            return Err("base64 re-encoding differs from the file".into());
        }
        return Ok(vec![value.to_xdr(Limits::none()).map_err(|e| e.to_string())?]);
    }
    let bytes = fs::read(path).map_err(|e| e.to_string())?;
    Ok(split_frames(&bytes)?.into_iter().map(<[u8]>::to_vec).collect())
}

fn envelopes(lcm: &LedgerCloseMeta) -> Vec<&TransactionEnvelope> {
    let set = match lcm {
        LedgerCloseMeta::V0(m) => return m.tx_set.txs.iter().collect(),
        LedgerCloseMeta::V1(m) => &m.tx_set,
        LedgerCloseMeta::V2(m) => &m.tx_set,
    };
    let GeneralizedTransactionSet::V1(set) = set;
    let mut out = Vec::new();
    for phase in set.phases.iter() {
        match phase {
            TransactionPhase::V0(components) => {
                for TxSetComponent::TxsetCompTxsMaybeDiscountedFee(c) in components.iter() {
                    out.extend(c.txs.iter());
                }
            }
            TransactionPhase::V1(parallel) => {
                for stage in parallel.execution_stages.iter() {
                    for cluster in stage.0.iter() {
                        out.extend(cluster.0.iter());
                    }
                }
            }
        }
    }
    out
}

fn tx_set_json(lcm: &LedgerCloseMeta, network_id: [u8; 32]) -> Result<serde_json::Value, String> {
    let mut out = Vec::new();
    for env in envelopes(lcm) {
        let hash = env.hash(network_id).map_err(|e| e.to_string())?;
        let hex: String = hash.iter().map(|b| format!("{b:02x}")).collect();
        out.push(serde_json::json!({"hash": hex, "envelope": env}));
    }
    Ok(serde_json::Value::Array(out))
}

fn check_record(file: &str, i: usize, raw: &[u8], network_id: [u8; 32]) -> (bool, serde_json::Value) {
    let value = match LedgerCloseMeta::from_xdr(raw, Limits::none()) {
        Ok(v) => v,
        Err(e) => {
            return (false, serde_json::json!({"file": file, "record": i, "decode": "error", "error": e.to_string()}))
        }
    };
    let identical = value.to_xdr(Limits::none()).map(|b| b == raw).unwrap_or(false);
    match tx_set_json(&value, network_id) {
        Ok(tx_set) => (
            identical,
            serde_json::json!({"file": file, "record": i, "decode": "ok",
                               "roundTripIdentical": identical, "txSet": tx_set, "value": value}),
        ),
        Err(e) => (false, serde_json::json!({"file": file, "record": i, "decode": "error", "error": e})),
    }
}

fn main() -> ExitCode {
    let args: Vec<String> = env::args().skip(1).collect();
    if args.len() < 3 || args[0] != "--network-passphrase" {
        eprintln!("usage: lcm-goldens --network-passphrase <passphrase> <file.xdr|file.b64>...");
        return ExitCode::from(2);
    }
    let network_id: [u8; 32] = Sha256::digest(args[1].as_bytes()).into();
    let stdout = std::io::stdout();
    let mut out = std::io::BufWriter::new(stdout.lock());
    let mut ok = true;
    for file in &args[2..] {
        let recs = match records(file) {
            Ok(r) if !r.is_empty() => r,
            other => {
                let error = other.err().unwrap_or_else(|| "no records".to_string());
                ok = false;
                writeln!(out, "{}", serde_json::json!({"file": file, "record": 0, "decode": "error", "error": error})).unwrap();
                continue;
            }
        };
        for (i, raw) in recs.iter().enumerate() {
            let (good, line) = check_record(file, i, raw, network_id);
            ok &= good;
            writeln!(out, "{line}").unwrap();
        }
    }
    out.flush().unwrap();
    if ok { ExitCode::SUCCESS } else { ExitCode::from(1) }
}
