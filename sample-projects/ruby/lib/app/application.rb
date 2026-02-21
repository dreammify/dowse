# typed: strict
# frozen_string_literal: true

require_relative '../core/string_utils'
require_relative '../models/address'
require_relative '../models/permission'
require_relative 'user_service'

module App
  class Application
    extend T::Sig

    sig { void }
    def self.run
      repository = Models::UserRepository.new
      service = UserService.new(repository)

      result = service.create_user("john doe", "john@example.com", Models::Permission::Admin)
      if result.success?
        puts "User created successfully"
      else
        failure = T.cast(result, Core::Failure)
        puts "Error: #{failure.error}"
      end

      address = Models::Address.new(
        street: "123 Main St",
        city: "springfield",
        postal_code: "12345",
        country: "US"
      )
      puts "City: #{Core::StringUtils.capitalize_words(address.city)}"
      puts "Street: #{Core::StringUtils.truncate(address.street, 10)}"

      admins = service.list_admins
      puts "Admins: #{admins.length}"
    end
  end
end
