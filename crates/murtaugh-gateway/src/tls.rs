/// Picks rustls's crypto backend for the whole process. Dependencies compile in both `ring` and
/// `aws-lc-rs`, and with two available rustls refuses to guess and panics on the first TLS dial.
pub fn init() {
    let _ = rustls::crypto::ring::default_provider().install_default();
}

#[cfg(test)]
mod tests {
    #[test]
    fn a_tls_client_can_be_built_once_the_provider_is_chosen() {
        super::init();
        super::init();
        let _ = rustls::ClientConfig::builder();
    }
}
