//! Probe contract for the XDR shape corpus. Deployed to testnet only to make core emit shapes
//! that the built-in contracts cannot: an arbitrary `ScVal` argument, return value and event body,
//! and an authorization tree with a sub-invocation.
#![no_std]
use soroban_sdk::{contract, contractimpl, symbol_short, token, Address, Env, Val};

#[contract]
pub struct Probe;

#[contractimpl]
impl Probe {
    /// Returns `value` unchanged and publishes it as the data of an `echo` event, so the same
    /// `ScVal` appears in the invocation arguments, the return value and a contract event.
    pub fn echo(env: Env, value: Val) -> Val {
        #[allow(deprecated)]
        env.events().publish((symbol_short!("echo"),), value);
        value
    }

    /// `from` authorizes this call, which then calls `transfer` on the token contract `sac`.
    /// The transfer needs `from`'s authorization as well, so the authorization entry for `from`
    /// is a tree: `pay` at the root with `transfer` as its sub-invocation.
    pub fn pay(env: Env, from: Address, sac: Address, to: Address, amount: i128) {
        from.require_auth();
        token::TokenClient::new(&env, &sac).transfer(&from, &to, &amount);
    }
}
