use crate::models::User;

pub struct Store {
    users: Vec<User>,
}

impl Store {
    pub fn new() -> Self {
        Store { users: Vec::new() }
    }

    pub fn add(&mut self, user: User) {
        self.users.push(user);
    }

    pub fn count(&self) -> u32 {
        self.users.len() as u32
    }
}
