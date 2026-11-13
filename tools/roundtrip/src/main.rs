//! Usage: xdr-roundtrip <XdrTypeName> <file.b64> [--json]
//! Decodes base64 XDR as the named type, re-encodes it and reports whether the bytes are identical.
use std::{env, fs, process::ExitCode, str::FromStr};
use stellar_xdr::{Limits, Type, TypeVariant, WriteXdr};

fn main() -> ExitCode {
    let args: Vec<String> = env::args().collect();
    if args.len() < 3 {
        eprintln!("usage: xdr-roundtrip <Type> <file.b64> [--json]");
        return ExitCode::from(2);
    }
    let variant = match TypeVariant::from_str(&args[1]) {
        Ok(v) => v,
        Err(e) => { eprintln!("unknown type {}: {e:?}", args[1]); return ExitCode::from(2); }
    };
    let b64 = fs::read_to_string(&args[2]).expect("read file").trim().to_string();
    let value = match Type::from_xdr_base64(variant, &b64, Limits::none()) {
        Ok(v) => v,
        Err(e) => { println!("{{\"type\":\"{}\",\"decode\":\"error\",\"error\":\"{e}\"}}", args[1]); return ExitCode::from(1); }
    };
    let again = value.to_xdr_base64(Limits::none()).expect("encode");
    let identical = again == b64;
    if args.iter().any(|a| a == "--json") {
        println!("{}", serde_json::to_string(&value).expect("json"));
    } else {
        println!("{{\"type\":\"{}\",\"decode\":\"ok\",\"roundTripIdentical\":{identical}}}", args[1]);
    }
    if identical { ExitCode::SUCCESS } else { ExitCode::from(1) }
}
