package com.example.app

import com.example.core.Entity
import com.example.core.Identifiable
import com.example.core.Result
import com.example.core.capitalizeWords
import com.example.core.getOrNull
import com.example.models.Permission
import com.example.models.User
import com.example.models.UserRepository

class UserService(private val repository: UserRepository) {

    fun createUser(name: String, email: String, permission: Permission): Result<User> {
        val normalizedName = name.capitalizeWords()
        val user = User(
            id = System.currentTimeMillis(),
            name = normalizedName,
            email = email,
            permission = permission
        )
        return repository.save(user)
    }

    fun getUser(id: Long): User? = repository.findById(id).getOrNull()

    fun listAdmins(): List<User> =
        repository.findAll().filter { it.permission == Permission.ADMIN }

    fun <T> validateEntity(entity: Entity): Boolean = entity.isValid()

    fun <T> describeIdentifiable(item: Identifiable<T>): String =
        "Item with id=${item.id}"
}
