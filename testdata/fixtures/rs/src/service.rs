use crate::models::User;
use crate::store::Store;
use crate::util::{clamp, helper as h};

pub struct Service {
    store: Store,
    user: User,
}

pub trait Describe {
    fn describe(&self) -> String;
}

impl Describe for Service {
    fn describe(&self) -> String {
        self.user.greet()
    }
}

impl Service {
    pub fn new(store: Store, user: User) -> Self {
        Self { store, user }
    }

    pub fn make_default(store: Store, user: User) -> Self {
        Self::new(store, user)
    }

    pub fn total(&self) -> u32 {
        let s: &Store = &self.store;
        clamp(s.count(), 0, 100) + h(1)
    }

    pub fn label(&self) -> String {
        self.user.greet()
    }

    pub fn summary(&self) -> u32 {
        self.total()
    }
}
