//! Usage: xdr-checks <sightings.json>
//! Runs the API checks of sightings.json (entries with "inputs") against rs-stellar-xdr and prints
//! a JSON array of {sighting, check, result, detail}, where result is "defect", "ok" or "n/a".
use serde_json::{json, Value};
use std::{env, fs, str::FromStr};
use stellar_xdr::{
    AccountId, AlphaNum4, Asset, AssetCode4, ScBytes, ScMap, ScMapEntry, ScString, ScSymbol, ScVal,
};

// js-stellar-sdk#1766: assets with the same code order by issuer key bytes.
// stellar-xdr has no pool API; Asset derives Ord, which is what sorts a pair.
fn asset_pair_order(inputs: &Value) -> (&'static str, String) {
    let asset = |issuer: &str| {
        Asset::CreditAlphanum4(AlphaNum4 {
            asset_code: AssetCode4::from_str(inputs["code"].as_str().unwrap()).unwrap(),
            issuer: AccountId::from_str(issuer).unwrap(),
        })
    };
    let first = asset(inputs["issuerFirstByBytes"].as_str().unwrap());
    let second = asset(inputs["issuerSecondByBytes"].as_str().unwrap());
    if first < second {
        ("ok", "Asset's Ord puts the first issuer by key bytes first".into())
    } else {
        ("defect", "Asset's Ord puts the second issuer by key bytes first".into())
    }
}

fn sc_val(key: &Value) -> ScVal {
    let (kind, v) = key.as_object().unwrap().iter().next().unwrap();
    match kind.as_str() {
        "u32" => ScVal::U32(v.as_u64().unwrap() as u32),
        "i32" => ScVal::I32(v.as_i64().unwrap() as i32),
        "bytes" => {
            let hex = v.as_str().unwrap();
            let raw: Vec<u8> = (0..hex.len()).step_by(2).map(|i| u8::from_str_radix(&hex[i..i + 2], 16).unwrap()).collect();
            ScVal::Bytes(ScBytes(raw.try_into().unwrap()))
        }
        "string" => ScVal::String(ScString(v.as_str().unwrap().try_into().unwrap())),
        "symbol" => ScVal::Symbol(ScSymbol(v.as_str().unwrap().try_into().unwrap())),
        other => panic!("unknown key type {other}"),
    }
}

// js-stellar-sdk#1766: the sorted-map constructor puts keys in the order the Soroban host requires.
fn scmap_key_order(inputs: &Value) -> (&'static str, String) {
    let keys = inputs["keys"].as_array().unwrap();
    let want = inputs["hostOrder"].as_array().unwrap();
    let entries = keys.iter().map(|k| ScMapEntry { key: sc_val(k), val: ScVal::Void });
    let map = ScMap::sorted_from(entries).unwrap();
    for (i, (entry, w)) in map.0.iter().zip(want).enumerate() {
        if entry.key != sc_val(w) {
            let got = keys.iter().find(|k| sc_val(k) == entry.key).unwrap();
            return ("defect", format!("ScMap::sorted_from puts {got} at position {}; host order has {w}", i + 1));
        }
    }
    ("ok", format!("ScMap::sorted_from puts all {} keys in host order", want.len()))
}

fn main() {
    let path = env::args().nth(1).expect("usage: xdr-checks <sightings.json>");
    let sightings: Value = serde_json::from_str(&fs::read_to_string(path).unwrap()).unwrap();
    let mut out = Vec::new();
    for s in sightings["sightings"].as_array().unwrap().iter().filter(|s| s.get("inputs").is_some()) {
        let check = s["check"].as_str().unwrap();
        let (result, detail) = match check {
            "asset-pair-ordered-by-issuer-bytes" => asset_pair_order(&s["inputs"]),
            "scmap-keys-sorted-like-host" => scmap_key_order(&s["inputs"]),
            "muxed-id-encodes-the-id-given" => ("n/a", "MuxedAccountMed25519.id is a u64 and muxed strkeys parse into it; no API takes an ID as text".into()),
            "offer-id-encodes-the-id-given" => ("n/a", "ManageSellOfferOp.offer_id is a plain Int64 and stellar-xdr has no operation builder to validate it".into()),
            other => panic!("no Rust check for {other}"),
        };
        out.push(json!({"sighting": s["id"], "check": check, "result": result, "detail": detail}));
    }
    println!("{}", serde_json::to_string(&out).unwrap());
}
