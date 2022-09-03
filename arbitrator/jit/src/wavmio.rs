// Copyright 2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

use std::{
    io,
    io::{BufReader, ErrorKind},
    net::TcpStream,
    sync::Arc,
};

use parking_lot::MutexGuard;

use crate::{
    gostack::{Escape, GoStack, Inbox, MaybeEscape, WasmEnv, WasmEnvArc},
    socket,
};

pub type Bytes32 = [u8; 32];

pub fn get_global_state_bytes32(env: &WasmEnvArc, sp: u32) -> MaybeEscape {
    let (sp, mut env) = GoStack::new(sp, env);
    ready_hostio(&mut *env)?;

    let global = sp.read_u64(0) as u32 as usize;
    let out_ptr = sp.read_u64(1);
    let mut out_len = sp.read_u64(2) as usize;
    if out_len < 32 {
        eprintln!("Go trying to read block hash into {out_len} bytes long buffer");
    } else {
        out_len = 32;
    }

    let global = match env.large_globals.get(global) {
        Some(global) => global,
        None => return Escape::hostio("global read out of bounds in wavmio.getGlobalStateBytes32"),
    };
    sp.write_slice(out_ptr, &global[..out_len]);
    Ok(())
}

pub fn set_global_state_bytes32(env: &WasmEnvArc, sp: u32) -> MaybeEscape {
    let (sp, mut env) = GoStack::new(sp, env);
    ready_hostio(&mut *env)?;

    let global = sp.read_u64(0) as u32 as usize;
    let src_ptr = sp.read_u64(1);
    let src_len = sp.read_u64(2);
    if src_len != 32 {
        eprintln!("Go trying to set 32-byte global with a {src_len} bytes long buffer");
        return Ok(());
    }

    let slice = sp.read_slice(src_ptr, src_len);
    let slice = &slice.try_into().unwrap();
    match env.large_globals.get_mut(global) {
        Some(global) => *global = *slice,
        None => {
            return Escape::hostio("global write out of bounds in wavmio.setGlobalStateBytes32")
        }
    }
    Ok(())
}

pub fn get_global_state_u64(env: &WasmEnvArc, sp: u32) -> MaybeEscape {
    let (sp, mut env) = GoStack::new(sp, env);
    ready_hostio(&mut *env)?;

    let global = sp.read_u64(0) as u32 as usize;
    match env.small_globals.get(global) {
        Some(global) => sp.write_u64(1, *global),
        None => return Escape::hostio("global read out of bounds in wavmio.getGlobalStateU64"),
    }
    Ok(())
}

pub fn set_global_state_u64(env: &WasmEnvArc, sp: u32) -> MaybeEscape {
    let (sp, mut env) = GoStack::new(sp, env);
    ready_hostio(&mut *env)?;

    let global = sp.read_u64(0) as u32 as usize;
    match env.small_globals.get_mut(global) {
        Some(global) => *global = sp.read_u64(1),
        None => return Escape::hostio("global write out of bounds in wavmio.setGlobalStateU64"),
    }
    Ok(())
}

pub fn read_inbox_message(env: &WasmEnvArc, sp: u32) -> MaybeEscape {
    let (sp, mut env) = GoStack::new(sp, env);
    ready_hostio(&mut *env)?;

    let inbox = &env.sequencer_messages;
    inbox_message_impl(&sp, &env, inbox, "wavmio.readInboxMessage")
}

pub fn read_delayed_inbox_message(env: &WasmEnvArc, sp: u32) -> MaybeEscape {
    let (sp, mut env) = GoStack::new(sp, env);
    ready_hostio(&mut *env)?;

    let inbox = &env.delayed_messages;
    inbox_message_impl(&sp, &env, inbox, "wavmio.readDelayedInboxMessage")
}

/// Reads an inbox message
/// note: the order of the checks is very important.
fn inbox_message_impl(
    sp: &GoStack,
    env: &MutexGuard<WasmEnv>,
    inbox: &Inbox,
    name: &str,
) -> MaybeEscape {
    let msg_num = sp.read_u64(0);
    let offset = sp.read_u64(1);
    let out_ptr = sp.read_u64(2);
    let out_len = sp.read_u64(3);
    if out_len != 32 {
        eprintln!("Go trying to read inbox message with out len {out_len} in {name}");
        sp.write_u64(5, 0);
        return Ok(());
    }

    macro_rules! error {
        ($text:expr $(,$args:expr)*) => {{
            let text = format!($text $(,$args)*);
            return Escape::hostio(&text)
        }};
    }

    let too_far = env.first_too_far;
    let message = match inbox.get(&msg_num) {
        Some(message) => message,
        None => match msg_num < too_far {
            true => error!("missing inbox message {msg_num} of {too_far} in {name}"),
            false => error!("message {msg_num} of {too_far} too far in {name}"),
        },
    };

    if out_ptr + 32 > sp.memory_size() {
        error!("unknown message type in {name}");
    }
    let offset = match u32::try_from(offset) {
        Ok(offset) => offset as usize,
        Err(_) => error!("bad offset {offset} in {name}"),
    };

    let len = std::cmp::min(32, message.len().saturating_sub(offset)) as usize;
    let read = message.get(offset..(offset + len)).unwrap_or_default();
    sp.write_slice(out_ptr, &read);
    sp.write_u64(5, read.len() as u64);
    Ok(())
}

pub fn resolve_preimage(env: &WasmEnvArc, sp: u32) -> MaybeEscape {
    let (sp, env) = GoStack::new(sp, env);
    let name = "wavmio.resolvePreImage";

    let hash_ptr = sp.read_u64(0);
    let hash_len = sp.read_u64(1);
    let offset = sp.read_u64(3);
    let out_ptr = sp.read_u64(4);
    let out_len = sp.read_u64(5);
    if hash_len != 32 || out_len != 32 {
        eprintln!("Go trying to resolve pre image with hash len {hash_len} and out len {out_len}");
        sp.write_u64(7, 0);
        return Ok(());
    }

    macro_rules! error {
        ($text:expr $(,$args:expr)*) => {{
            let text = format!($text $(,$args)*);
            return Escape::hostio(&text)
        }};
    }

    let hash = sp.read_slice(hash_ptr, hash_len);
    let hash: &[u8; 32] = &hash.try_into().unwrap();
    let preimage = match env.preimages.get(hash) {
        Some(preimage) => preimage,
        None => error!(
            "Missing requested preimage for hash {} in {name}",
            hex::encode(hash)
        ),
    };
    let offset = match u32::try_from(offset) {
        Ok(offset) => offset as usize,
        Err(_) => error!("bad offset {offset} in {name}"),
    };

    let len = std::cmp::min(32, preimage.len().saturating_sub(offset)) as usize;
    let read = preimage.get(offset..(offset + len)).unwrap_or_default();
    sp.write_slice(out_ptr, &read);
    sp.write_u64(7, read.len() as u64);
    Ok(())
}

fn ready_hostio(env: &mut WasmEnv) -> MaybeEscape {
    if !env.forks {
        return Ok(());
    }

    let stdin = io::stdin();
    let mut port = String::new();

    loop {
        if let Err(error) = stdin.read_line(&mut port) {
            return match error.kind() {
                ErrorKind::UnexpectedEof => Escape::exit(0),
                error => Escape::hostio(&format!("Error reading stdin: {error}")),
            };
        }

        unsafe {
            match libc::fork() {
                -1 => return Escape::hostio("Failed to fork"),
                0 => break,                // we're the child process
                _ => port = String::new(), // we're the parent process
            }
        }
    }

    let address = format!("127.0.0.1:{port}");
    let socket = TcpStream::connect(&address)?;

    let mut reader = BufReader::new(socket);
    let stream = &mut reader;

    let inbox_position = socket::read_u64(stream)?;
    let position_within_message = socket::read_u64(stream)?;
    let last_block_hash = socket::read_bytes32(stream)?;
    let last_send_root = socket::read_bytes32(stream)?;

    env.small_globals = vec![inbox_position, position_within_message];
    env.large_globals = vec![last_block_hash, last_send_root];

    env.sequencer_messages.clear();
    env.delayed_messages.clear();

    let mut inbox_position = inbox_position;
    let mut delayed_position = socket::read_u64(stream)?;

    while socket::read_u8(stream)? == 1 {
        let message = socket::read_bytes(stream)?;
        env.sequencer_messages.insert(inbox_position, message);
        inbox_position += 1;
    }
    while socket::read_u8(stream)? == 1 {
        let message = socket::read_bytes(stream)?;
        env.delayed_messages.insert(delayed_position, message);
        delayed_position += 1;
    }

    env.socket = Arc::new(Some(reader));
    env.forks = false;
    Ok(())
}
