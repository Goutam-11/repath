macro_rules! blind {
    () => {
        hidden_call()
    };
}

pub fn invoke() {
    blind!();
}
